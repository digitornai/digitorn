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

type chatLLM interface {
	Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

type contextCompactor struct {
	store  *sessionstore.Bus
	apps   appmgr.Manager
	llm    chatLLM
	logger *slog.Logger
	touch     func(sessionID string)
	touchSync func(sessionID string)

	nonBlocking bool
	prepare func(sessionID, jwt string)
}

func newContextCompactor(store *sessionstore.Bus, apps appmgr.Manager, client chatLLM, logger *slog.Logger) *contextCompactor {
	return &contextCompactor{store: store, apps: apps, llm: client, logger: logger}
}

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

	msgBudget := contextMessageBudget(cfg, brain, snap)

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

	postMessages := contextcompact.EstimateTokens(res.Messages)
	tokensFreed := tokensBefore - postMessages
	if tokensFreed < 0 {
		tokensFreed = 0
	}
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
	if c.prepare != nil {
		c.prepare(sessionID, llm.UserJWTFromContext(ctx))
	}
	return nil
}

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

func contextMessageBudget(cfg *schema.ContextConfig, brain schema.Brain, snap sessionstore.SessionSnapshot) int {
	window, reserved := 0, 0
	if cfg != nil {
		window = cfg.MaxTokens
		reserved = cfg.OutputReserved
	}
	if window <= 0 {
		window = contextcompact.ContextWindowFor(brain.Provider, brain.Model)
	}
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

func (c *contextCompactor) resolveContextConfig(ctx context.Context, appID, sessionID string) (*schema.ContextConfig, schema.Brain, bool) {
	var brain schema.Brain
	if c.apps == nil || appID == "" {
		return nil, brain, false
	}
	app, err := c.apps.Get(ctx, appID)
	if err != nil || app == nil || app.Definition == nil {
		return nil, brain, false
	}
	byok := app.Meta != nil && app.Meta.BYOK
	var appCfg *schema.ContextConfig
	if app.Definition.Runtime != nil {
		appCfg = app.Definition.Runtime.Context
	}
	if len(app.Definition.Agents) > 0 {
		brain = app.Definition.Agents[0].Brain
	}
	eff := mergeContextConfig(appCfg, brain.Context)
	if eff != nil && eff.SummaryBrain != nil {
		brain = *eff.SummaryBrain
	}
	if sessionID != "" && brain.Model != "" {
		if state, err := c.store.State(sessionID); err == nil && state != nil {
			snap := state.Snapshot()
			agentID := ""
			if len(app.Definition.Agents) > 0 {
				agentID = app.Definition.Agents[0].ID
			}
			if ovr, ok := snap.ModelOverrides[agentID]; ok && ovr != "" {
				brain.Model = ovr
				if eff != nil {
					eff.MaxTokens = 0
				}
			}
		}
	}
	return eff, brain, byok
}

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

func buildSummarizerPrompt(maxTokens int) string {
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

type llmSummarizer struct {
	client    chatLLM
	brain     schema.Brain
	sessionID string
	byok bool
	userJWT string
	logger *slog.Logger
}

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

func renderTranscript(msgs []sessionstore.Message) string {
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
