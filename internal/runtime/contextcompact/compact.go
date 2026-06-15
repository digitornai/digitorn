// Package contextcompact implements LLM context-window compaction —
// keeping an agent's prompt under the model's token budget by rewriting
// the conversation history. Reference :
// docs-site/docs/language/06-context-management.md.
//
// This is DISTINCT from sessionstore compaction (which snapshots the
// JSONL to disk). Here we reduce what the LLM SEES, not what is stored :
// the full history stays on disk for audit ; the LLM just receives a
// compacted view ([summary/recap] + the most-recent messages).
//
// Two strategies, both documented :
//
//   - truncate  : drop the older slice, inject a recap system message.
//     No LLM call ; can never fail ; the reliability floor.
//   - summarize : summarise the dropped slice via a (cheap) summary
//     brain, inject the summary, keep the recent slice. One
//     LLM call ; FALLS BACK to truncate if the call fails so
//     compaction is ALWAYS guaranteed to make progress.
//
// The cardinal safety rule (enforced by SafeSplitIndex) : a compaction
// must NEVER strand a tool result whose tool_call landed in the dropped
// slice — that produces an "orphan tool_use_id" provider error. The
// split point is moved earlier until the kept slice is self-consistent.
package contextcompact

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// Strategy values mirror schema.ContextStrategy.
const (
	StrategyTruncate  = "truncate"
	StrategySummarize = "summarize"
)

// defaultKeepRecent is the doc default for keep_recent.
const defaultKeepRecent = 10

// charsPerToken is the documented estimation heuristic (~4 chars/token).
const charsPerToken = 4

// SafeSplitIndex returns the index `cut` into msgs such that msgs[cut:]
// are the messages to KEEP verbatim and msgs[:cut] are the messages to
// drop/summarise. Guarantees :
//
//   - keeps AT LEAST keepRecent messages (cut <= len-keepRecent), but
//   - moves cut EARLIER (keeping more) whenever the kept slice would
//     contain a tool result whose matching tool_call is in the dropped
//     prefix — so the kept conversation is always self-consistent.
//   - never splits inside a single message.
//
// Returns 0 when nothing should be dropped (<= keepRecent messages, or
// keeping tool pairs intact forces us back to the start). A 0 result
// means "compaction is a no-op this pass" — the caller leaves history
// untouched, which is the safe outcome.
func SafeSplitIndex(msgs []sessionstore.Message, keepRecent int) int {
	if keepRecent <= 0 {
		keepRecent = 1
	}
	n := len(msgs)
	if n <= keepRecent {
		return 0
	}
	cut := n - keepRecent
	// Pull the split earlier until the kept slice has no orphan tool
	// result. Each step adds one message to the kept slice, so this
	// terminates at cut==0 in the worst case (keep everything).
	for cut > 0 && hasOrphanToolResult(msgs[cut:]) {
		cut--
	}
	return cut
}

// hasOrphanToolResult reports whether the slice contains a tool result
// whose tool_call id is NOT present in the slice (i.e. its call was
// dropped). Both the structured ToolResult.ToolCallID and the message's
// ToolCallIDs back-reference are considered.
func hasOrphanToolResult(slice []sessionstore.Message) bool {
	opened := make(map[string]struct{})
	for i := range slice {
		for _, p := range slice[i].Parts {
			if p.Type == sessionstore.PartTypeToolCall && p.ToolCall != nil && p.ToolCall.ID != "" {
				opened[p.ToolCall.ID] = struct{}{}
			}
		}
	}
	for i := range slice {
		for _, p := range slice[i].Parts {
			if p.Type == sessionstore.PartTypeToolResult && p.ToolResult != nil && p.ToolResult.ToolCallID != "" {
				if _, ok := opened[p.ToolResult.ToolCallID]; !ok {
					return true
				}
			}
		}
		// A "tool" message may carry its links only via ToolCallIDs.
		if slice[i].Role == "tool" {
			for _, id := range slice[i].ToolCallIDs {
				if id == "" {
					continue
				}
				if _, ok := opened[id]; !ok {
					return true
				}
			}
		}
	}
	return false
}

// Result is the outcome of a compaction pass.
type Result struct {
	// Messages is the compacted view : [recap/summary system message] +
	// the kept recent slice. When Dropped == 0 it equals the input.
	Messages []sessionstore.Message
	// Dropped is how many leading messages were compacted away.
	Dropped int
	// CutoffSeq is the Seq of the last dropped message (0 when none).
	// The durable layer records this so the LLM view can be reproduced
	// after a resume by skipping messages with Seq <= CutoffSeq.
	CutoffSeq uint64
	// Summary is the synthesised recap injected as a system message.
	Summary string
	// Strategy actually applied ("truncate" or "summarize"). May differ
	// from the requested one when summarize fell back to truncate.
	Strategy string
}

// Truncate is the cheap, never-fails path : drop msgs[:cut] and prepend
// a recap system message built from the dropped slice + goal. No LLM.
func Truncate(msgs []sessionstore.Message, keepRecent int, goal string) Result {
	cut := SafeSplitIndex(msgs, keepRecent)
	if cut == 0 {
		return Result{Messages: msgs, Dropped: 0, Strategy: StrategyTruncate}
	}
	recap := BuildReminder(msgs[:cut], goal)
	return Result{
		Messages:  prependSystem(recap, msgs[cut:]),
		Dropped:   cut,
		CutoffSeq: msgs[cut-1].Seq,
		Summary:   recap,
		Strategy:  StrategyTruncate,
	}
}

// Summarizer turns a dropped slice into a compact recap via an LLM
// (the summary brain). maxTokens caps the synthesised summary.
type Summarizer interface {
	Summarize(ctx context.Context, dropped []sessionstore.Message, maxTokens int) (string, error)
}

// Summarize is the smart path : summarise the dropped slice via s and
// inject it as a system message. If s is nil OR the summary call fails,
// it FALLS BACK to Truncate so compaction always makes progress — the
// agent never gets wedged because a summary model was slow or down.
func Summarize(ctx context.Context, msgs []sessionstore.Message, keepRecent int, s Summarizer, maxTokens int, goal, priorSummary string) Result {
	cut := SafeSplitIndex(msgs, keepRecent)
	if cut == 0 {
		return Result{Messages: msgs, Dropped: 0, Strategy: StrategySummarize}
	}
	if s == nil {
		return Truncate(msgs, keepRecent, goal)
	}
	// The dropped slice is the WINDOW being compacted further. When the window no
	// longer carries the pre-window history (it was trimmed to the prior cutoff),
	// the prior summary is fed as leading context so the NEW summary is cumulative
	// — summary-of-summary + the newly-dropped messages — instead of forgetting
	// everything before the previous compaction.
	dropped := msgs[:cut]
	// Feed the UNFRAMED recap as prior : the stored summary is framed with
	// imperative agent instructions ("resume the mission", "do not restate this
	// recap"), and a summary brain re-reading them obeys them — collapsing the
	// cumulative summary to a stub and forgetting everything before this pass.
	if rec := unframeHandoff(priorSummary); strings.TrimSpace(rec) != "" {
		pri := sessionstore.Message{
			Role:    "system",
			Content: rec,
			Parts:   []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: rec}},
		}
		dropped = append([]sessionstore.Message{pri}, dropped...)
	}
	summary, err := s.Summarize(ctx, dropped, maxTokens)
	if err != nil || strings.TrimSpace(summary) == "" {
		// Reliability : a failed/empty summary must not block compaction.
		return Truncate(msgs, keepRecent, goal)
	}
	// Anti-collapse / fact-preservation : a re-summarisation must never silently
	// drop a concrete fact the prior carried. Some models, distracted by a narrow
	// recent exchange, emit a degenerate stub or quietly forget a value. When the
	// new summary collapsed in length OR lost a distinctive prior fact token, keep
	// the richer prior (the cutoff still advances over the new messages — only the
	// latest, least-critical turns drop, never an accumulated fact).
	if prior := strings.TrimSpace(unframeHandoff(priorSummary)); prior != "" {
		_, lost := summaryDroppedFact(prior, summary)
		if lost || len(strings.TrimSpace(summary)) < len(prior)/2 {
			summary = prior
		}
	}
	// First-pass backstop : recover any concrete fact the LLM dropped from the
	// dropped conversation itself (no prior summary protects the very first pass).
	summary = appendMissingSourceFacts(summary, dropped)
	body := frameHandoff(summary)
	return Result{
		Messages:  prependSystem(body, msgs[cut:]),
		Dropped:   cut,
		CutoffSeq: msgs[cut-1].Seq,
		Summary:   body,
		Strategy:  StrategySummarize,
	}
}

// compactionIntro and compactionOutro frame every compaction handoff with
// the runtime's authoritative voice. The contract is seamlessness : the agent
// must resume as if the compaction never happened and the user must not be
// able to tell one occurred. Durable working memory (goal/tasks/facts) is
// re-injected separately every turn, so the handoff defers to it instead of
// duplicating it.
const (
	compactionIntro = "Context checkpoint — earlier turns were compacted to save space, but NOTHING is lost to you. The recap below IS your complete memory of this conversation and contains everything you need to continue the task: treat every fact, name, number, identifier, decision and user request in it as something you personally saw and still remember from the live conversation, and rely on it for any detail. It is high-fidelity conversation memory — NOT a <digitorn-directive>, NOT confidential runtime state. Use it freely and directly, exactly as if the full history were still in front of you."
	compactionOutro = "When the user refers to or asks about anything captured above, answer straight from this recap as if you simply remembered it — never say it wasn't mentioned or that you have no record of it. Just don't announce to the user that a compaction happened, and don't paste the recap back verbatim. Then continue the mission seamlessly."
)

// frameHandoff wraps a recap (an LLM summary or the deterministic reminder)
// in the checkpoint envelope so the model sees it as authoritative runtime
// context, not as conversational text.
func frameHandoff(recap string) string {
	return compactionIntro + "\n\n<recap>\n" + strings.TrimSpace(recap) + "\n</recap>\n\n" + compactionOutro
}

// unframeHandoff recovers the bare recap from a framed handoff so it can be
// re-fed to the summary brain as prior context without its agent-directed
// intro/outro. Returns the input trimmed when it carries no <recap> envelope
// (an already-bare summary, e.g. a background-prepared one).
func unframeHandoff(s string) string {
	const open, closeTag = "<recap>", "</recap>"
	i := strings.Index(s, open)
	j := strings.LastIndex(s, closeTag)
	if i >= 0 && j > i {
		return strings.TrimSpace(s[i+len(open) : j])
	}
	return strings.TrimSpace(s)
}

var notableTokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._/-]*[A-Za-z0-9]`)

// keyFactsSection returns the text of the recap's KEY FACTS block (the dense,
// verbatim fact list) — from the header to the next section — or "" when absent.
func keyFactsSection(s string) string {
	low := strings.ToLower(s)
	start := strings.Index(low, "key facts")
	if start < 0 {
		return ""
	}
	end := len(s)
	for _, h := range []string{"mission", "task &", "task and", "progress", "files &", "files and", "open items", "pitfalls"} {
		if i := strings.Index(low[start+9:], h); i >= 0 {
			if cand := start + 9 + i; cand < end {
				end = cand
			}
		}
	}
	return s[start:end]
}

// isStructuredToken reports whether a token is a concrete, data-like fact (a
// number, a code/path/identifier, a camelCase or ALLCAPS name) rather than a
// plain English word — the kind a summary must never silently drop.
func isStructuredToken(w string) bool {
	if len(w) < 2 {
		return false
	}
	var digits, letters, upper int
	lowerThenUpper, joiner, prevLower := false, false, false
	for _, r := range w {
		switch {
		case r >= '0' && r <= '9':
			digits++
			prevLower = false
		case r >= 'A' && r <= 'Z':
			upper++
			letters++
			if prevLower {
				lowerThenUpper = true
			}
			prevLower = false
		case r >= 'a' && r <= 'z':
			letters++
			prevLower = true
		case r == '.' || r == '_' || r == '-' || r == '/':
			joiner = true
			prevLower = false
		default:
			prevLower = false
		}
	}
	switch {
	case digits >= 2: // 47, 2048
		return true
	case digits >= 1 && letters >= 1: // ORCHID-9, v2
		return true
	case lowerThenUpper: // EventLoop
		return true
	case upper >= 2 && letters > upper: // SQLite
		return true
	case upper >= 3 && upper == letters: // SQL, HTTP
		return true
	case joiner && len(w) >= 5: // orchid.db, rate_limiter.py
		return true
	}
	return false
}

// priorFactTokens collects the distinctive fact tokens a re-summarisation must
// preserve. Scoped to the KEY FACTS block when present (bounded, high-signal, so
// the narrative can still evolve); falls back to the whole recap otherwise. Path
// tokens are reduced to their basename so `./data/x.db` and `x.db` match.
func priorFactTokens(prior string) map[string]struct{} {
	set := map[string]struct{}{}
	scope := keyFactsSection(prior)
	if strings.TrimSpace(scope) == "" {
		scope = prior
	}
	for _, w := range notableTokenRe.FindAllString(scope, -1) {
		if i := strings.LastIndexByte(w, '/'); i >= 0 && i < len(w)-1 {
			w = w[i+1:]
		}
		if isStructuredToken(w) {
			set[strings.ToLower(w)] = struct{}{}
		}
	}
	return set
}

// summaryDroppedFact reports the first prior fact token missing from next — i.e.
// the re-summarisation silently forgot a concrete fact.
func summaryDroppedFact(prior, next string) (string, bool) {
	nl := strings.ToLower(next)
	for tok := range priorFactTokens(prior) {
		if !strings.Contains(nl, tok) {
			return tok, true
		}
	}
	return "", false
}

// structuredTokensIn collects the distinctive fact tokens (numbers, identifiers,
// paths reduced to basename) appearing in s.
func structuredTokensIn(s string, into map[string]struct{}) {
	for _, w := range notableTokenRe.FindAllString(s, -1) {
		if i := strings.LastIndexByte(w, '/'); i >= 0 && i < len(w)-1 {
			w = w[i+1:]
		}
		if isStructuredToken(w) {
			into[strings.ToLower(w)] = struct{}{}
		}
	}
}

// StripKeyFactsSection removes the KEY FACTS block from a recap. The facts live
// in the lossless working-memory channel (injected separately every turn), so the
// recap injected to the model carries only the NARRATIVE — the model never sees a
// fact twice and the compacted prompt genuinely shrinks. The stored recap keeps
// its KEY FACTS (the cumulative-merge substrate); only the injected copy is
// stripped. Returns s unchanged when there is no KEY FACTS block.
func StripKeyFactsSection(s string) string {
	kf := keyFactsSection(s)
	if strings.TrimSpace(kf) == "" {
		return s
	}
	out := strings.Replace(s, kf, "", 1)
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}

var bulletRe = regexp.MustCompile(`^\s*(?:[-*•]\s+|\d+[.)]\s+)`)

// cleanFactLine strips a leading bullet/number marker and markdown emphasis from
// a KEY FACTS line, returning the bare fact (or "" for the header / too-short).
func cleanFactLine(s string) string {
	s = bulletRe.ReplaceAllString(strings.TrimSpace(s), "")
	s = strings.TrimSpace(strings.ReplaceAll(s, "**", ""))
	if len(s) < 3 || strings.HasPrefix(strings.ToUpper(s), "KEY FACTS") {
		return ""
	}
	return s
}

// ExtractNewKeyFacts pulls the concrete facts from a summary's KEY FACTS section
// that are NOT already covered by `existing` (the session's lossless structured
// fact channel). A line counts as new only when it carries a distinctive token
// (number, identifier, path) absent from every existing fact — so re-summarising
// the same facts each compaction never re-adds them. This lets the compactor
// promote the summariser's KEY FACTS into the never-compacted working memory
// without the agent having to record them explicitly.
func ExtractNewKeyFacts(summary string, existing []string) []string {
	kf := keyFactsSection(summary)
	if strings.TrimSpace(kf) == "" {
		return nil
	}
	seen := map[string]struct{}{}
	for _, f := range existing {
		structuredTokensIn(f, seen)
	}
	var out []string
	for _, raw := range strings.Split(kf, "\n") {
		line := cleanFactLine(raw)
		if line == "" {
			continue
		}
		toks := map[string]struct{}{}
		structuredTokensIn(line, toks)
		if len(toks) == 0 {
			continue // narrative line, no concrete value to preserve losslessly
		}
		fresh := false
		for tok := range toks {
			if _, ok := seen[tok]; !ok {
				fresh = true
				break
			}
		}
		if !fresh {
			continue
		}
		out = append(out, line)
		for tok := range toks {
			seen[tok] = struct{}{}
		}
	}
	return out
}

// appendMissingSourceFacts is the first-pass backstop : it guarantees no concrete
// fact STATED IN THE DROPPED CONVERSATION is lost when the LLM omits it — even on
// the very first compaction, where there is no prior summary to fall back to. Any
// structured fact present in the dropped messages but absent from the summary is
// appended verbatim (mechanical, deterministic, no LLM). It fires only on a real
// drop, so a complete summary is returned unchanged.
func appendMissingSourceFacts(summary string, dropped []sessionstore.Message) string {
	want := map[string]struct{}{}
	for i := range dropped {
		if dropped[i].Role == "system" { // the prepended prior recap, not source
			continue
		}
		structuredTokensIn(messageText(dropped[i]), want)
	}
	if len(want) == 0 {
		return summary
	}
	sl := strings.ToLower(summary)
	var missing []string
	for tok := range want {
		if !strings.Contains(sl, tok) {
			missing = append(missing, tok)
		}
	}
	if len(missing) == 0 {
		return summary
	}
	sort.Strings(missing)
	return summary + "\n\nADDITIONAL FACTS (preserved verbatim from the conversation): " + strings.Join(missing, ", ")
}

// BuildReminder builds the recap injected by truncate (and used as the
// summarize fallback). With no LLM available it carries what is known
// deterministically — the goal and a short tail of the dropped activity —
// inside the same checkpoint envelope as the summarize path, so even the
// no-LLM floor hands off with the "continue seamlessly" framing. Pure +
// deterministic.
func BuildReminder(dropped []sessionstore.Message, goal string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d earlier message(s) were elided to stay within the token budget. No LLM summary was produced for this checkpoint; the notes below are what is known deterministically.", len(dropped)))
	if g := strings.TrimSpace(goal); g != "" {
		b.WriteString("\nGoal: ")
		b.WriteString(g)
	}
	// Tail recap : the last few non-empty user/assistant snippets from
	// the dropped slice, so the agent remembers what just happened.
	const tail = 3
	snips := recentSnippets(dropped, tail)
	if len(snips) > 0 {
		b.WriteString("\nRecent activity before compaction:")
		for _, s := range snips {
			b.WriteString("\n- ")
			b.WriteString(s)
		}
	}
	return frameHandoff(b.String())
}

// recentSnippets returns up to n short one-line snippets from the most
// recent user/assistant messages in the slice (oldest→newest order).
func recentSnippets(msgs []sessionstore.Message, n int) []string {
	var picked []string
	for i := len(msgs) - 1; i >= 0 && len(picked) < n; i-- {
		m := msgs[i]
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		txt := strings.TrimSpace(messageText(m))
		if txt == "" {
			continue
		}
		picked = append(picked, fmt.Sprintf("%s: %s", m.Role, clip(txt, 160)))
	}
	// Reverse to chronological order.
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked
}

// EstimateTokens approximates the token cost of msgs using the
// documented ~4-chars-per-token heuristic over all textual content.
func EstimateTokens(msgs []sessionstore.Message) int {
	chars := 0
	for i := range msgs {
		chars += len(messageText(msgs[i]))
		chars += len(msgs[i].Role) + 4 // per-message framing overhead
	}
	return chars / charsPerToken
}

// messageText extracts the textual content of a message WITHOUT
// double-counting. The projection mirrors a message's text in both
// Content and a text Part, so we prefer Parts when present and fall back
// to Content only when there are no text-bearing parts.
func messageText(m sessionstore.Message) string {
	var b strings.Builder
	sawPartText := false
	for _, p := range m.Parts {
		switch p.Type {
		case sessionstore.PartTypeText:
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(p.Text)
				sawPartText = true
			}
		case sessionstore.PartTypeToolResult:
			if p.ToolResult != nil {
				for _, rp := range p.ToolResult.Parts {
					if rp.Type == sessionstore.PartTypeText && rp.Text != "" {
						if b.Len() > 0 {
							b.WriteByte(' ')
						}
						b.WriteString(rp.Text)
						sawPartText = true
					}
				}
			}
		}
	}
	if !sawPartText && m.Content != "" {
		return m.Content
	}
	return b.String()
}

func prependSystem(text string, rest []sessionstore.Message) []sessionstore.Message {
	out := make([]sessionstore.Message, 0, len(rest)+1)
	out = append(out, sessionstore.Message{
		Role:    "system",
		Content: text,
		Parts:   []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: text}},
	})
	out = append(out, rest...)
	return out
}

func clip(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	// Trim back to a rune boundary so a multi-byte rune is never split (a raw
	// s[:max] can cut a UTF-8 sequence and emit an invalid � byte).
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ApplyView reduces a full message history to the COMPACTED VIEW the
// model should see, given a durable compaction marker : every message
// with Seq <= cutoffSeq is hidden and replaced by a single `summary`
// system message at the front. Used by the engine each turn to build
// the prompt ; the on-disk history is never modified, so the view is
// reproducible after a resume from the persisted marker.
//
// cutoffSeq == 0 means "no compaction" → the input is returned as-is.
// Messages with Seq == 0 (not yet sequenced) are always kept so a
// freshly-injected message is never accidentally dropped.
func ApplyView(msgs []sessionstore.Message, cutoffSeq uint64, summary string) []sessionstore.Message {
	if cutoffSeq == 0 {
		return msgs
	}
	out := make([]sessionstore.Message, 0, len(msgs)+1)
	if strings.TrimSpace(summary) != "" {
		out = append(out, sessionstore.Message{
			Role:    "system",
			Content: summary,
			Parts:   []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: summary}},
		})
	}
	for _, m := range msgs {
		if m.Seq == 0 || m.Seq > cutoffSeq {
			out = append(out, m)
		}
	}
	return out
}

// ApplyPrepared validates and applies a background-PREPARED cutoff to the
// CURRENT message set (CTX-8). It hides every message with Seq <= cutoffSeq,
// prepends summary, and returns the compacted view + how many were dropped.
// ok=false — the caller must fall back to truncate — when cutoffSeq is 0,
// nothing would be dropped, or applying it would strand an orphan tool result
// in the kept slice (its tool_call dropped). The orphan check runs against the
// LIVE messages, so a stale prepared cutoff can never produce an invalid prompt.
func ApplyPrepared(msgs []sessionstore.Message, cutoffSeq uint64, summary string) (view []sessionstore.Message, dropped int, ok bool) {
	if cutoffSeq == 0 {
		return nil, 0, false
	}
	kept := make([]sessionstore.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq == 0 || m.Seq > cutoffSeq {
			kept = append(kept, m)
		} else {
			dropped++
		}
	}
	if dropped == 0 || hasOrphanToolResult(kept) {
		return nil, 0, false
	}
	if strings.TrimSpace(summary) == "" {
		return kept, dropped, true
	}
	return prependSystem(summary, kept), dropped, true
}

// KeepRecentOrDefault resolves keep_recent, falling back to the doc
// default (10) when unset/invalid.
func KeepRecentOrDefault(keepRecent int) int {
	if keepRecent <= 0 {
		return defaultKeepRecent
	}
	return keepRecent
}
