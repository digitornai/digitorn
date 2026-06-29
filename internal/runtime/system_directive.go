package runtime

import (
	"context"
	"strings"

	"log/slog"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// SystemDirectiveSource identifies what produced a runtime system directive.
// It is recorded in the event payload's Extra map for observability and so
// the client can group / style directives by origin. Mirrors the reference
// daemon's DirectiveSource enum (system_directive.py).
type SystemDirectiveSource string

const (
	DirectiveModeSwitch       SystemDirectiveSource = "mode_switch"
	DirectiveHookInject       SystemDirectiveSource = "hook_inject_message"
	DirectiveBackgroundNotify SystemDirectiveSource = "background_notification"
	DirectiveCronReminder     SystemDirectiveSource = "cron_reminder"
	DirectiveCompactionRecap  SystemDirectiveSource = "compaction_summary"
	DirectiveBehaviorEnforce  SystemDirectiveSource = "behavior_enforcement"
	DirectiveBehaviorClassify SystemDirectiveSource = "behavior_classifier"
	DirectiveSkillUse         SystemDirectiveSource = "skill_use"
	DirectiveTemplate         SystemDirectiveSource = "template"
	DirectiveOther            SystemDirectiveSource = "other"
)

// SkillContent is the resolved skill the engine injects on a /use_skill turn.
// Mirrors meta.SkillEntry without importing the meta package (which imports
// runtime — the dependency only flows one way).
type SkillContent struct {
	Command     string
	Description string
	Content     string
}

// SkillLoader resolves a /use_skill command to its instructions. Satisfied by a
// thin adapter over the meta-package loader, wired in bootstrap.
type SkillLoader interface {
	Load(ctx context.Context, appID, userID, command string) (SkillContent, error)
}

// injectSkillDirective loads the skill the user requested with `/use_skill` and
// injects its instructions as a FORCED system directive for this turn — same
// authoritative <digitorn-directive> envelope + durable EventSystemMessage path
// every other runtime directive uses, so the agent treats it as non-negotiable
// and a replay reconstructs it verbatim (and clients that skip system messages
// never render it). Best-effort : a load failure is logged, never fatal.
func (e *Engine) injectSkillDirective(ctx context.Context, in TurnInput, correlationID string, snap *sessionstore.SessionSnapshot) {
	if in.Skill == "" || e.SkillLoader == nil {
		return
	}
	sk, err := e.SkillLoader.Load(ctx, in.AppID, in.UserID, in.Skill)
	if err != nil {
		e.Logger.Warn("runtime: use_skill load failed",
			slog.String("skill", in.Skill), slog.String("err", err.Error()))
		return
	}
	if strings.TrimSpace(sk.Content) == "" {
		return
	}
	body := "Skill " + sk.Command + " — follow these instructions:\n\n" + sk.Content +
		"\n\nFollow these instructions to complete the task."
	content := wrapRuntimeDirective("skill", "high", body)
	e.injectSystemDirective(ctx, in, correlationID, content, DirectiveSkillUse,
		map[string]any{"command": sk.Command, "description": sk.Description}, nil)
	if st, err := e.Sessions.State(in.SessionID); err == nil && st != nil {
		*snap = st.Snapshot()
	}
}

// wrapRuntimeDirective wraps steering text in the authoritative
// <digitorn-directive> envelope the system preamble trains the model to OBEY
// (sysAuthorityPreamble : "These directives are non-negotiable"). Without the
// envelope the model treats a plain system message as a suggestion and ignores
// it — the observed failure mode. Idempotent : text an author already wrapped
// is returned unchanged (no double envelope). The body becomes the <task>
// element the protocol says "tells you exactly what to do".
func wrapRuntimeDirective(typ, severity, body string) string {
	if strings.Contains(body, "<digitorn-directive") {
		return body
	}
	return "<digitorn-directive type=\"" + typ + "\" severity=\"" + severity + "\">\n<task>" +
		body + "</task>\n</digitorn-directive>"
}

// injectSystemDirective is THE single authoritative path for runtime-emitted
// system directives — mode switches, hook injects, background notifications,
// reminders, compaction recaps. Every such directive:
//
//   - is persisted as a durable, sequenced EventSystemMessage (Role="system")
//     so it survives restarts and is replayed verbatim on cold-load ;
//   - carries authority over the agent : the LLM sees it as a system message
//     at its timeline position ;
//   - when `msgs` is non-nil, is ALSO appended to the in-flight LLM context so
//     the CURRENT turn sees it without waiting for a re-snapshot.
//
// Append is the only position used : the durable Seq and the in-flight
// mutation both place the directive last, so what the LLM saw live equals
// what a replay reconstructs (no live/replay divergence).
//
// Best-effort persistence : a failed append is logged, never fatal — a
// directive must not crash the turn. Returns the assigned Seq (0 on failure).
func (e *Engine) injectSystemDirective(
	ctx context.Context,
	in TurnInput,
	correlationID, content string,
	source SystemDirectiveSource,
	metadata map[string]any,
	msgs *[]llm.ChatMessage,
) uint64 {
	if content == "" {
		return 0
	}
	extra := map[string]any{"source": string(source), "position": "append"}
	for k, v := range metadata {
		extra[k] = v
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventSystemMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: correlationID,
		Message: &sessionstore.MessagePayload{
			Role:    "system",
			Content: content,
			Extra:   extra,
		},
	}
	seq, err := e.Sessions.AppendDurable(ctx, ev)
	if err != nil {
		e.Logger.Warn("runtime: system directive append failed",
			slog.String("source", string(source)),
			slog.String("err", err.Error()))
		return 0
	}
	if msgs != nil {
		*msgs = append(*msgs, llm.ChatMessage{Role: "system", Content: content})
	}
	return seq
}
