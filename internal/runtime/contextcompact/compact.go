package contextcompact

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const (
	StrategyTruncate  = "truncate"
	StrategySummarize = "summarize"
)

const defaultKeepRecent = 3

const charsPerToken = 4

func SafeSplitIndex(msgs []sessionstore.Message, keepRecent int) int {
	if keepRecent <= 0 {
		keepRecent = 1
	}
	n := len(msgs)
	if n <= keepRecent {
		return 0
	}
	cut := n - keepRecent
	for cut > 0 && hasOrphanToolResult(msgs[cut:]) {
		cut--
	}
	return cut
}

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

type Result struct {
	Messages []sessionstore.Message
	Dropped int
	CutoffSeq uint64
	Summary string
	Strategy string
}

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

type Summarizer interface {
	Summarize(ctx context.Context, dropped []sessionstore.Message, maxTokens int) (string, error)
}

func Summarize(ctx context.Context, msgs []sessionstore.Message, keepRecent int, s Summarizer, maxTokens int, goal, priorSummary string) Result {
	cut := SafeSplitIndex(msgs, keepRecent)
	if cut == 0 {
		return Result{Messages: msgs, Dropped: 0, Strategy: StrategySummarize}
	}
	if s == nil {
		return Truncate(msgs, keepRecent, goal)
	}
	dropped := msgs[:cut]
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
		return Truncate(msgs, keepRecent, goal)
	}
	if prior := strings.TrimSpace(unframeHandoff(priorSummary)); prior != "" {
		_, lost := summaryDroppedFact(prior, summary)
		if lost || len(strings.TrimSpace(summary)) < len(prior)/2 {
			summary = prior
		}
	}
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

const (
	compactionIntro = "Context checkpoint — earlier turns were compacted to save space. The recap below covers the OLDER history; the RECENT messages (including the user's current request) appear verbatim AFTER this recap in the conversation. Treat every fact, name, number, identifier, decision and user request in the recap as something you personally remember from the live conversation. It is high-fidelity conversation memory — NOT a <digitorn-directive>, NOT confidential runtime state."
	compactionOutro = "The recent messages that follow this recap are verbatim and authoritative — always respond to the ACTUAL latest message from the user, not to what the recap describes as \"last request\". Resume directly without acknowledging the compaction, without recapping, and without asking the user to repeat themselves. Continue as if the full history were still in front of you."
)

func frameHandoff(recap string) string {
	return compactionIntro + "\n\n<recap>\n" + strings.TrimSpace(recap) + "\n</recap>\n\n" + compactionOutro
}

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
	case digits >= 2:
		return true
	case digits >= 1 && letters >= 1:
		return true
	case lowerThenUpper:
		return true
	case upper >= 2 && letters > upper:
		return true
	case upper >= 3 && upper == letters:
		return true
	case joiner && len(w) >= 5:
		return true
	}
	return false
}

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

func summaryDroppedFact(prior, next string) (string, bool) {
	nl := strings.ToLower(next)
	for tok := range priorFactTokens(prior) {
		if !strings.Contains(nl, tok) {
			return tok, true
		}
	}
	return "", false
}

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

func cleanFactLine(s string) string {
	s = bulletRe.ReplaceAllString(strings.TrimSpace(s), "")
	s = strings.TrimSpace(strings.ReplaceAll(s, "**", ""))
	if len(s) < 3 || strings.HasPrefix(strings.ToUpper(s), "KEY FACTS") {
		return ""
	}
	return s
}

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
			continue
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

func appendMissingSourceFacts(summary string, dropped []sessionstore.Message) string {
	want := map[string]struct{}{}
	for i := range dropped {
		if dropped[i].Role == "system" {
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

func BuildReminder(dropped []sessionstore.Message, goal string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d earlier message(s) were elided to stay within the token budget. No LLM summary was produced for this checkpoint; the notes below are what is known deterministically.", len(dropped)))
	if g := strings.TrimSpace(goal); g != "" {
		b.WriteString("\nGoal: ")
		b.WriteString(g)
	}
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
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked
}

func EstimateTokens(msgs []sessionstore.Message) int {
	chars := 0
	for i := range msgs {
		chars += len(messageText(msgs[i]))
		chars += len(msgs[i].Role) + 4
	}
	return chars / charsPerToken
}

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
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

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

func KeepRecentOrDefault(keepRecent int) int {
	if keepRecent <= 0 {
		return defaultKeepRecent
	}
	return keepRecent
}
