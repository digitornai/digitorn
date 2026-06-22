package server

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/contextcompact"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// chatLLM is the slice of the LLM client the compactor needs for the
// summarize strategy. *llm.Client satisfies it.
type chatLLM interface {
	Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

// contextCompactor is the production SessionCompactor wired into the
// hook engine's ActionDeps. It turns a `compact_context` hook action
// into a REAL LLM-context compaction : it reads the session's history,
// resolves the app/agent ContextConfig (keep_recent / strategy /
// summary_brain / summary_max_tokens), runs the contextcompact engine,
// and emits a durable EventContextCompacted that the runtime applies to
// every subsequent prompt.
//
// Reliability contract : the summarize path is best-effort — if the
// summary brain is unreachable or returns nothing, contextcompact falls
// back to truncate, so compaction ALWAYS makes progress. The on-disk
// history is never modified ; only the model's view shrinks.
type contextCompactor struct {
	store  *sessionstore.Bus
	apps   appmgr.Manager
	llm    chatLLM
	logger *slog.Logger
	touch     func(sessionID string) // async recount (mid-turn updates)
	touchSync func(sessionID string) // synchronous recount (post-compaction exact count)

	// nonBlocking (CTX-8 feature flag) makes the summarize strategy NEVER call the
	// LLM on the turn loop: it applies a background-PREPARED summary when one is
	// ready and advances the cutoff, else truncates instantly. false = legacy
	// behaviour (inline LLM summarize, which can block the loop for seconds).
	nonBlocking bool
	// prepare nudges the background summary maintainer to (re)prepare a
	// high-fidelity summary for the session, carrying the turn's user JWT for
	// gateway-mode auth. nil = maintainer disabled (legacy path).
	prepare func(sessionID, jwt string)
}

func newContextCompactor(store *sessionstore.Bus, apps appmgr.Manager, client chatLLM, logger *slog.Logger) *contextCompactor {
	return &contextCompactor{store: store, apps: apps, llm: client, logger: logger}
}

// CompactSession implements hooks.SessionCompactor. strategy + keepLast
// come from the `compact_context` action params ; summary_brain /
// summary_max_tokens come from the resolved ContextConfig. A no-op
// (Dropped == 0) returns nil without emitting an event.
func (c *contextCompactor) CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error {
	if c == nil || c.store == nil || sessionID == "" {
		return nil
	}
	state, err := c.store.State(sessionID)
	if err != nil || state == nil {
		return err
	}
	snap := state.Snapshot()

	cfg, brain, byok := c.resolveContextConfig(ctx, snap.AppID, sessionID)

	keepRecent := keepLast
	if keepRecent <= 0 && cfg != nil {
		keepRecent = cfg.KeepRecent
	}
	keepRecent = contextcompact.KeepRecentOrDefault(keepRecent)

	// Always protect the last user message: it must never land in the dropped
	// portion, even when system/tool notifications were injected after it.
	// Find the last user message and extend keepRecent to include it.
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if snap.Messages[i].Role == "user" {
			if minKeep := len(snap.Messages) - i; keepRecent < minKeep {
				keepRecent = minKeep
			}
			break
		}
	}

	if strategy == "" && cfg != nil {
		strategy = string(cfg.Strategy)
	}
	if strategy == "" {
		strategy = contextcompact.StrategySummarize
	}
	summaryMax := 2048
	if cfg != nil && cfg.SummaryMaxTokens > 0 {
		summaryMax = cfg.SummaryMaxTokens
	}

	// Token budget for the kept conversation : the room left under the window
	// after the FIXED overhead (system prompt + tool schemas, which compaction
	// can't touch) plus a small recap margin. This is what lets truncate hold
	// the window when recent tool results are individually large — a fixed
	// keep_recent COUNT can't. The breakdown comes from the EXACT background
	// recount ; 0 (no recount yet) widens the budget harmlessly.
	msgBudget := contextMessageBudget(cfg, brain, snap)

	// Pre-check the deterministic safe-split on the SAME snapshot the strategy
	// will use. cut == 0 means there is nothing to compact this pass : we return
	// WITHOUT emitting any event, so a client never sees a "compacting…" start
	// with no matching end. The truncate path uses the token-budget split so a
	// big-tool-result tail is dropped even when the message COUNT is small.
	var cut int
	if strategy == contextcompact.StrategySummarize {
		cut = contextcompact.SafeSplitIndex(snap.Messages, keepRecent)
	} else {
		cut = contextcompact.SafeSplitIndexBudget(snap.Messages, keepRecent, msgBudget)
	}
	if cut == 0 {
		return nil
	}
	effectiveStrategy := contextcompact.StrategyTruncate
	if strategy == contextcompact.StrategySummarize {
		effectiveStrategy = contextcompact.StrategySummarize
	}
	tokensBefore := contextcompact.EstimateTokens(snap.Messages)

	// START marker : emitted BEFORE the (possibly slow, LLM-backed) work so
	// clients can show a "compacting…" indicator. It carries the cutoff +
	// dropped count known up-front from the split, plus the REQUESTED
	// strategy (the END marker reports what actually ran, which may differ
	// when summarize falls back to truncate). Durable → its Seq is strictly
	// below the END marker's, and the pair survives replay.
	if _, err = c.store.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventContextCompacting,
		SessionID: sessionID,
		AppID:     snap.AppID,
		UserID:    snap.UserID,
		CtxCompact: &sessionstore.ContextCompactPayload{
			CutoffSeq:       snap.Messages[cut-1].Seq,
			KeepRecent:      keepRecent,
			Strategy:        effectiveStrategy,
			MessagesDropped: cut,
			TokensBefore:    tokensBefore,
		},
	}); err != nil {
		return fmt.Errorf("compactor: persist context_compacting: %w", err)
	}

	var res contextcompact.Result
	switch strategy {
	case contextcompact.StrategySummarize:
		// Fast path : when the background maintainer already has a high-fidelity
		// summary ready, apply it instantly (no LLM call this turn). Fidelity is
		// never traded for it — when nothing is ready we summarise inline below,
		// not truncate. Summarising blocks only THIS agent's turn goroutine (a
		// snapshot copy, no shared lock), never the daemon or other sessions.
		applied := false
		if c.nonBlocking {
			if r, ok := c.tryApplyPrepared(snap); ok {
				res, applied = r, true
			}
		}
		if !applied {
			s := &llmSummarizer{client: c.llm, brain: brain, sessionID: sessionID, byok: byok, userJWT: llm.UserJWTFromContext(ctx), logger: c.logger}
			prior := ""
			if snap.ContextCompaction != nil {
				prior = snap.ContextCompaction.Summary
			}
			res = contextcompact.Summarize(ctx, snap.Messages, keepRecent, s, summaryMax, snap.Goal, prior)
		}
	default:
		res = contextcompact.TruncateBudget(snap.Messages, keepRecent, msgBudget, snap.Goal)
	}

	// Abort cancels EVERYTHING, including an in-flight compaction. If the turn ctx
	// Abort only when the context was cancelled AND no usable result was produced
	// (res.Dropped == 0). If truncate already ran and produced a valid cutoff,
	// apply it even if the LLM summarize was cancelled — the result is complete.
	if ctx.Err() != nil && res.Dropped == 0 {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_, _ = c.store.AppendDurable(dctx, sessionstore.Event{
			Type:      sessionstore.EventContextCompacted,
			SessionID: sessionID,
			AppID:     snap.AppID,
			UserID:    snap.UserID,
			CtxCompact: &sessionstore.ContextCompactPayload{
				CutoffSeq:       0,
				Strategy:        "aborted",
				MessagesDropped: 0,
				TokensBefore:    tokensBefore,
			},
		})
		if c.logger != nil {
			c.logger.Info("context compaction aborted (turn cancelled) — no cutoff applied",
				slog.String("session_id", sessionID))
		}
		return ctx.Err()
	}

	// The EXACT new occupancy still lands via the tokenizer worker recompute
	// (gauge). tokens_freed here is an INFORMATIONAL estimate of the CONVERSATION
	// dropped (before − the kept view) for the client's "N tokens freed" summary
	// — never the reported gauge.
	postMessages := contextcompact.EstimateTokens(res.Messages)
	tokensFreed := tokensBefore - postMessages
	if tokensFreed < 0 {
		tokensFreed = 0
	}
	// Full post-compaction occupancy = the FIXED overhead compaction never touches
	// (system prompt + tool schemas, from the EXACT background recount) + the kept
	// conversation. This is the number on the SAME scale as the window and the live
	// gauge, so a client's "ctx used/window" after compaction isn't a misleadingly
	// tiny messages-only figure. 0 system/tools (no recount yet) degrades it to the
	// kept-messages estimate, matching the client's fallback.
	newContextTokens := snap.ContextSystemTokens + snap.ContextToolsTokens + postMessages
	_, err = c.store.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventContextCompacted,
		SessionID: sessionID,
		AppID:     snap.AppID,
		UserID:    snap.UserID,
		CtxCompact: &sessionstore.ContextCompactPayload{
			CutoffSeq:        res.CutoffSeq,
			Summary:          res.Summary,
			KeepRecent:       keepRecent,
			Strategy:         res.Strategy,
			MessagesDropped:  res.Dropped,
			TokensBefore:     tokensBefore,
			TokensFreed:      tokensFreed,
			NewContextTokens: newContextTokens,
		},
	})
	if err != nil {
		return fmt.Errorf("compactor: persist context_compacted: %w", err)
	}
	if c.logger != nil {
		c.logger.Info("context compacted",
			slog.String("session_id", sessionID),
			slog.String("strategy", res.Strategy),
			slog.Int("dropped", res.Dropped),
			slog.Uint64("cutoff_seq", res.CutoffSeq))
	}
	// Auto-promote the summary's KEY FACTS into the lossless, never-compacted
	// working-memory channel (snap.Facts) so they survive verbatim across every
	// future compaction — independent of the (faithful but lossy) recap and
	// without the agent having to record them. Deduped against existing facts.
	// Best-effort : a fact-promotion failure must never fail the compaction.
	for _, fact := range contextcompact.ExtractNewKeyFacts(res.Summary, snap.Facts) {
		if _, ferr := c.store.AppendDurable(ctx, sessionstore.Event{
			Type:      sessionstore.EventMemoryFactAdded,
			SessionID: sessionID,
			AppID:     snap.AppID,
			UserID:    snap.UserID,
			Memory:    &sessionstore.MemoryPayload{Fact: fact},
		}); ferr != nil && c.logger != nil {
			c.logger.Warn("compactor: key-fact promotion failed (non-fatal)",
				slog.String("session_id", sessionID), slog.String("err", ferr.Error()))
			break
		}
	}
	if c.touch != nil {
		c.touch(sessionID)
	}
	// CTX-8 : nudge the maintainer to prepare a fresh high-fidelity summary for
	// the NEXT compaction (off the loop), carrying the turn's JWT for gateway auth.
	if c.prepare != nil {
		c.prepare(sessionID, llm.UserJWTFromContext(ctx))
	}
	return nil
}

// tryApplyPrepared is the CTX-8 fast path : when the background maintainer has a
// high-fidelity summary ready that advances the cutoff AND is orphan-safe against
// the live messages, apply it instantly (no LLM call this turn). Returns ok=false
// when nothing usable is ready — the caller then summarises inline, never
// truncates, so fidelity is preserved regardless of maintainer timing.
func (c *contextCompactor) tryApplyPrepared(snap sessionstore.SessionSnapshot) (contextcompact.Result, bool) {
	prep := snap.PreparedSummary
	if prep == nil {
		return contextcompact.Result{}, false
	}
	curCutoff := uint64(0)
	if snap.ContextCompaction != nil {
		curCutoff = snap.ContextCompaction.CutoffSeq
	}
	if prep.CoversSeq <= curCutoff {
		return contextcompact.Result{}, false
	}
	view, dropped, ok := contextcompact.ApplyPrepared(snap.Messages, prep.CoversSeq, prep.Summary)
	if !ok {
		return contextcompact.Result{}, false
	}
	return contextcompact.Result{
		Messages:  view,
		Dropped:   dropped,
		CutoffSeq: prep.CoversSeq,
		Summary:   prep.Summary,
		Strategy:  contextcompact.StrategySummarize,
	}, true
}

// contextMessageBudget is the token room left for the kept conversation : the
// usable input budget (window − output_reserved) minus the FIXED overhead
// (system prompt + tool schemas, from the EXACT recount) and a small recap
// margin. truncate keeps recent messages within this, so total occupancy stays
// under the window even when tool results are individually large. Floored at
// 512 so a turn always keeps a little recent conversation.
func contextMessageBudget(cfg *schema.ContextConfig, brain schema.Brain, snap sessionstore.SessionSnapshot) int {
	window, reserved := 0, 0
	if cfg != nil {
		window = cfg.MaxTokens
		reserved = cfg.OutputReserved
	}
	if window <= 0 {
		window = contextcompact.ContextWindowFor(brain.Provider, brain.Model)
	}
	// EntryModelWindow is the authoritative real window persisted at model-switch
	// time. Use it when the resolved window is the generic ContextWindowFor
	// fallback (i.e. model not in hardcoded table) so truncate targets the correct
	// budget, not an 8k placeholder.
	if snap.EntryModelWindow > 0 && window == contextcompact.ContextWindowFor(brain.Provider, brain.Model) {
		window = snap.EntryModelWindow
	}
	if reserved <= 0 {
		reserved = 4096
	}
	limit := window
	if limit > reserved {
		limit -= reserved
	}
	budget := limit - snap.ContextSystemTokens - snap.ContextToolsTokens - 256
	if budget < 512 {
		budget = 512
	}
	return budget
}

// resolveContextConfig returns the effective ContextConfig and the brain
// to use for summarisation (summary_brain when set, else the agent's
// main brain). Per-agent brain.context overrides app-level runtime.context
// field-by-field (doc _resolve_context_config). nil cfg = no config.
// When sessionID is provided, the agent's ModelOverride (from PUT /sessions/{id}/model)
// is applied so the compactor uses the same model as the conversation.
func (c *contextCompactor) resolveContextConfig(ctx context.Context, appID, sessionID string) (*schema.ContextConfig, schema.Brain, bool) {
	var brain schema.Brain
	if c.apps == nil || appID == "" {
		return nil, brain, false
	}
	app, err := c.apps.Get(ctx, appID)
	if err != nil || app == nil || app.Definition == nil {
		return nil, brain, false
	}
	// BYOK routing mirrors the main turn (engine.go) : when the app is NOT in
	// BYOK mode, the summary call must NOT send the embedded sentinel key — it
	// goes through the gateway with the user's token (carried on the turn ctx),
	// exactly like the agent's own LLM calls. Sending BYOK with a placeholder key
	// is what made summarize silently fall back to truncate in gateway mode.
	byok := app.Meta != nil && app.Meta.BYOK
	var appCfg *schema.ContextConfig
	if app.Definition.Runtime != nil {
		appCfg = app.Definition.Runtime.Context
	}
	if len(app.Definition.Agents) > 0 {
		brain = app.Definition.Agents[0].Brain
	}
	eff := mergeContextConfig(appCfg, brain.Context)
	// Summary brain : explicit summary_brain wins, else the main brain.
	if eff != nil && eff.SummaryBrain != nil {
		brain = *eff.SummaryBrain
	}
	// Apply per-session model override so compactor uses the same model
	// the conversation is running on (respects PUT /sessions/{id}/model).
	if sessionID != "" && brain.Model != "" {
		if state, err := c.store.State(sessionID); err == nil && state != nil {
			snap := state.Snapshot()
			agentID := ""
			if len(app.Definition.Agents) > 0 {
				agentID = app.Definition.Agents[0].ID
			}
			if ovr, ok := snap.ModelOverrides[agentID]; ok && ovr != "" {
				brain.Model = ovr
				// Clear the YAML's context.max_tokens so contextMessageBudget
				// falls through to ContextWindowFor(ovr) — the real model's window.
				if eff != nil {
					eff.MaxTokens = 0
				}
			}
		}
	}
	return eff, brain, byok
}

// mergeContextConfig overlays the per-agent config onto the app-level
// one (agent non-zero fields win). Either may be nil.
func mergeContextConfig(app, agent *schema.ContextConfig) *schema.ContextConfig {
	if app == nil && agent == nil {
		return nil
	}
	out := schema.ContextConfig{}
	if app != nil {
		out = *app
	}
	if agent != nil {
		if agent.MaxTokens != 0 {
			out.MaxTokens = agent.MaxTokens
		}
		if agent.OutputReserved != 0 {
			out.OutputReserved = agent.OutputReserved
		}
		if agent.Strategy != "" {
			out.Strategy = agent.Strategy
		}
		if agent.KeepRecent != 0 {
			out.KeepRecent = agent.KeepRecent
		}
		if agent.CompressionTrigger != 0 {
			out.CompressionTrigger = agent.CompressionTrigger
		}
		if agent.SummaryMaxTokens != 0 {
			out.SummaryMaxTokens = agent.SummaryMaxTokens
		}
		if agent.SummaryBrain != nil {
			out.SummaryBrain = agent.SummaryBrain
		}
	}
	return &out
}

// buildSummarizerPrompt drives the summary brain. The goal is a dense,
// resumable HANDOFF — not a vague paragraph — so the agent can continue as if
// the compaction never happened. The sections mirror what an agent needs to
// pick the task back up: mission, plan, progress, files, open items, pitfalls.
// "MERGE not append" keeps the structure stable across successive compactions
// (the prior recap is fed back in as leading context).
//
// The word budget is BAKED INTO the prompt (derived from maxTokens) so the
// model self-regulates and emits a COMPLETE handoff that fits — instead of
// running freely and getting cut off mid-sentence by the MaxTokens cap, which
// would eat the LAST sections (OPEN ITEMS / PITFALLS / next step), the very
// ones the agent needs to resume. MaxTokens stays as a hard safety net ABOVE
// this stated budget. The budget also keeps the single cumulative summary
// bounded across successive compactions (it is re-summarised to the same size).
func buildSummarizerPrompt(maxTokens int) string {
	// ~0.7 words per token, floored, so the stated word budget sits comfortably
	// under the token cap and the cap never truncates a well-behaved response.
	words := maxTokens * 7 / 10
	if words < 80 {
		words = 80
	}
	return fmt.Sprintf(`You are compacting an AI agent's working session. You are seeing ONLY the OLDER portion of the conversation — the agent will receive your summary followed by the RECENT messages verbatim (those recent messages are NOT shown to you here). Your summary must capture everything the agent needs to understand the full context when it reads both your summary AND the recent messages that follow.

This handoff will become the agent's complete memory of the compacted history. Anything you omit is lost forever. Capture EVERYTHING needed to continue seamlessly; when in doubt, KEEP it.

Produce a dense, factual HANDOFF — never a vague paragraph. Use EXACTLY these sections, each a short header followed by tight bullet points. Omit a section only if it is truly empty:

KEY FACTS: every concrete fact, value, or instruction the user stated or asked to be remembered — codewords, credentials, identifiers, names, numbers, dates, URLs, file paths, exact values, requirements, preferences, decisions, agreements, and explicit constraints — recorded VERBATIM. MANDATORY: never drop, paraphrase, or compress these.
LAST USER REQUEST: the exact verbatim text of the LAST thing the user asked or requested in this compacted portion. Copy it word-for-word. MANDATORY even if it seems simple or already captured elsewhere.
MISSION: the user's overall goal and intent in their own words.
TASK & PLAN: what is being worked on right now, the chosen approach/strategy, and key design decisions.
PROGRESS: what has been done, what now works, and key decisions made and WHY.
FILES & ARTIFACTS: files or resources created/modified, their role, and important paths or contents.
OPEN ITEMS: what remains, blockers, unanswered questions, and the immediate next step.
PITFALLS: errors hit and how they were fixed, hard constraints, and things to avoid.

LENGTH BUDGET: aim to keep the handoff under about %d words so it is COMPLETE and never cut off mid-sentence. If everything does not fit, compress WORDING — never drop information: tighten PROGRESS/FILES/PITFALLS phrasing first, but ALWAYS keep KEY FACTS complete and verbatim, plus MISSION, OPEN ITEMS and the immediate next step.

Preserve concrete specifics: names, paths, identifiers, numbers, exact user requirements and wording. Invent nothing that is not in the conversation. If the input begins with a prior recap, MERGE it with the newer messages into one up-to-date handoff — do not just append, and carry EVERY prior KEY FACT and open item forward UNCHANGED (re-summarising must never lose a fact an earlier pass kept). No preamble, no sign-off — output only the sections.`, words)
}

// llmSummarizer calls the summary brain to recap a dropped slice. It
// flattens the slice to a plain-text transcript (no tool structure sent
// to the model — avoids tool-pairing errors) and asks for a terse recap.
// Any failure returns an error so the core falls back to truncate.
type llmSummarizer struct {
	client    chatLLM
	brain     schema.Brain
	sessionID string
	// byok mirrors the app's BYOK routing : true → send the embedded key ;
	// false (gateway mode) → send no key so the call uses the gateway with the
	// user's token, exactly like the agent's own LLM calls.
	byok bool
	// userJWT is the gateway bearer (gateway mode) carried from the turn ctx ;
	// without it the gateway call has no auth and bifrost mis-routes / rejects.
	userJWT string
	// logger surfaces WHY a summarize degraded to truncate. contextcompact
	// swallows the error (it falls back so compaction always progresses), so
	// without this a silent degradation is invisible in production.
	logger *slog.Logger
}

// summarizeFailed logs the cause of a summarize→truncate degradation. Best-effort.
func (s *llmSummarizer) summarizeFailed(err error) {
	if s.logger == nil {
		return
	}
	s.logger.Warn("compactor: summarize degraded to truncate",
		slog.String("session_id", s.sessionID),
		slog.String("provider", s.brain.Provider),
		slog.String("model", s.brain.Model),
		slog.Bool("byok", s.byok),
		slog.Bool("user_jwt", s.userJWT != ""),
		slog.String("err", err.Error()))
}

func (s *llmSummarizer) Summarize(ctx context.Context, dropped []sessionstore.Message, maxTokens int) (string, error) {
	if s.client == nil || s.brain.Provider == "" || s.brain.Model == "" {
		err := fmt.Errorf("summarizer: no summary brain configured")
		s.summarizeFailed(err)
		return "", err
	}
	var apiKey, baseURL string
	if s.byok {
		apiKey, baseURL = embeddedBrainAuth(s.brain)
	}
	transcript := renderTranscript(dropped)
	if strings.TrimSpace(transcript) == "" {
		err := fmt.Errorf("summarizer: empty transcript")
		s.summarizeFailed(err)
		return "", err
	}
	mt := maxTokens
	req := &llm.ChatRequest{
		BYOK:      s.byok,
		Provider:  s.brain.Provider,
		Model:     s.brain.Model,
		APIKey:    apiKey,
		BaseURL:   baseURL,
		UserJWT:   s.userJWT,
		MaxTokens: &mt,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: buildSummarizerPrompt(mt)},
			{Role: "user", Content: "Compact the earlier conversation below into the handoff. If it begins with a prior recap, MERGE it with the newer messages into one up-to-date handoff — do not append.\n\n" + transcript},
		},
		CorrelationID: "compact:" + s.sessionID,
	}
	resp, err := s.client.Chat(ctx, req)
	if err != nil {
		s.summarizeFailed(err)
		return "", err
	}
	if resp == nil {
		err := fmt.Errorf("summarizer: nil response")
		s.summarizeFailed(err)
		return "", err
	}
	if strings.TrimSpace(resp.Content) == "" {
		s.summarizeFailed(fmt.Errorf("summarizer: empty content (finish=%q)", resp.FinishReason))
	}
	return resp.Content, nil
}

// renderTranscript flattens messages for the summary brain (CTX-8 Phase 3).
// Beyond plain text it describes the actual WORK done — which tools were called
// (name + arg summary) and what they returned (named via the call id, with any
// error) — so the handoff captures decisions and outcomes, not just chatter.
// Big tool outputs are clipped so the summariser input stays bounded.
func renderTranscript(msgs []sessionstore.Message) string {
	// call id → tool name, so a result can name the tool that produced it.
	names := map[string]string{}
	for i := range msgs {
		for _, p := range msgs[i].Parts {
			if p.Type == sessionstore.PartTypeToolCall && p.ToolCall != nil && p.ToolCall.ID != "" {
				names[p.ToolCall.ID] = p.ToolCall.Name
			}
		}
	}
	var b strings.Builder
	for i := range msgs {
		if line := transcriptLine(msgs[i], names); line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func transcriptLine(m sessionstore.Message, names map[string]string) string {
	if res := toolResultOf(m); res != nil {
		var b strings.Builder
		b.WriteString("tool")
		if n := names[res.ToolCallID]; n != "" {
			b.WriteString(" ")
			b.WriteString(n)
		}
		b.WriteString(" result: ")
		if res.Error != "" {
			b.WriteString("[ERROR] ")
			b.WriteString(clipStr(res.Error, 300))
			b.WriteString(" ")
		}
		b.WriteString(clipStr(strings.TrimSpace(plainText(m)), 2000))
		return strings.TrimSpace(b.String())
	}
	txt := strings.TrimSpace(plainText(m))
	calls := toolCallLines(m)
	if txt == "" && len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.Role)
	b.WriteString(": ")
	b.WriteString(txt)
	for _, c := range calls {
		b.WriteString("\n  → called ")
		b.WriteString(c)
	}
	return strings.TrimSpace(b.String())
}

func toolResultOf(m sessionstore.Message) *sessionstore.ToolResultSpec {
	for _, p := range m.Parts {
		if p.Type == sessionstore.PartTypeToolResult && p.ToolResult != nil {
			return p.ToolResult
		}
	}
	return nil
}

func toolCallLines(m sessionstore.Message) []string {
	var out []string
	for _, p := range m.Parts {
		if p.Type == sessionstore.PartTypeToolCall && p.ToolCall != nil && p.ToolCall.Name != "" {
			out = append(out, p.ToolCall.Name+"("+argsSummary(p.ToolCall.Args)+")")
		}
	}
	return out
}

func argsSummary(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(clipStr(fmt.Sprintf("%v", args[k]), 80))
	}
	return clipStr(b.String(), 300)
}

func clipStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func plainText(m sessionstore.Message) string {
	var parts []string
	sawPart := false
	for _, p := range m.Parts {
		switch p.Type {
		case sessionstore.PartTypeText:
			if p.Text != "" {
				parts = append(parts, p.Text)
				sawPart = true
			}
		case sessionstore.PartTypeToolResult:
			if p.ToolResult != nil {
				for _, rp := range p.ToolResult.Parts {
					if rp.Type == sessionstore.PartTypeText && rp.Text != "" {
						parts = append(parts, rp.Text)
						sawPart = true
					}
				}
			}
		}
	}
	if !sawPart && m.Content != "" {
		return m.Content
	}
	return strings.Join(parts, " ")
}

// embeddedBrainAuth pulls (apiKey, baseURL) from a brain's declarative
// config — same shape extractEmbeddedAuth uses for agent brains.
func embeddedBrainAuth(b schema.Brain) (apiKey, baseURL string) {
	if s, ok := b.Config["api_key"].(string); ok && s != "" {
		apiKey = s
	}
	if apiKey == "" {
		if s, ok := b.Credential.(string); ok && s != "" {
			apiKey = s
		} else if m, ok := b.Credential.(map[string]any); ok {
			if s, ok := m["api_key"].(string); ok && s != "" {
				apiKey = s
			}
		}
	}
	if s, ok := b.Config["base_url"].(string); ok && s != "" {
		baseURL = s
	}
	return apiKey, baseURL
}
