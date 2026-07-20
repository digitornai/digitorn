package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// SubAgentSpec describes one delegated sub-agent run.
type SubAgentSpec struct {
	AppID         string
	ParentSession string
	UserID        string

	// UserJWT is the coordinator's gateway bearer, forwarded so the isolated
	// sub-turn can reach the gateway in gateway mode. Transient ; never seeded
	// into the sub-session's durable events.
	UserJWT string

	// AgentID is the target agent's logical (YAML) id ; it must be declared
	// in the app. RunID is the distinct instance id used on the wire + in
	// telemetry ; "" derives one from AgentID.
	AgentID string
	RunID   string

	// Task is the instruction the sub-agent works on. MemorySeed is the
	// read-only context (goal / facts) the coordinator briefs it with — it
	// is injected as a leading system message in the isolated sub-session,
	// never as access to the parent's history.
	Task       string
	MemorySeed string

	// Depth is the delegation depth (0 = spawned by the entry agent). The
	// AgentManager enforces a max depth ; the runner just carries it.
	Depth int

	// InheritContext, when true, seeds this run with the parent session's
	// rendered transcript (the "fork" mode). Default false = today's exact
	// isolated sub-agent behavior — nothing changes for existing callers.
	InheritContext bool
}

// AgentResult is the structured outcome a coordinator collects from a
// sub-agent. It is JSON-serialisable so it can be handed back to the LLM and
// streamed to clients.
type AgentResult struct {
	RunID      string `json:"run_id"`
	AgentID    string `json:"agent_id"`
	Session    string `json:"session"`
	Status     string `json:"status"` // "completed" | "errored"
	Content    string `json:"content"`
	Error      string `json:"error,omitempty"`
	TokensIn   int64  `json:"tokens_in"`
	TokensOut  int64  `json:"tokens_out"`
	DurationMs int64  `json:"duration_ms"`
}

// RunSubAgent runs a target agent as a fully ISOLATED sub-turn :
//
//   - a fresh sub-session (the sub-agent never sees the parent's history —
//     hard cross-session isolation),
//   - seeded with a read-only memory seed (system) + the task (user),
//   - executed OFF the user-turn pool (SubAgent: true) so nested delegation
//     can never deadlock on a full pool,
//   - projected back into a structured AgentResult.
//
// The sub-agent's own turn events live in its sub-session, isolated from the
// parent ; the coordinator only ever sees the returned AgentResult.
func (e *Engine) RunSubAgent(ctx context.Context, spec SubAgentSpec) (AgentResult, error) {
	runID := spec.RunID
	if runID == "" {
		runID = NewAgentRunID(spec.AgentID)
	}
	subSession := subSessionID(spec.ParentSession, runID)

	res := AgentResult{RunID: runID, AgentID: spec.AgentID, Session: subSession, Status: "errored"}
	start := time.Now()

	// Fork mode : hérite le contexte du parent (transcript rendu, borné).
	// Ne s'exécute QUE si InheritContext → le spawn normal est intact.
	if spec.InheritContext && spec.ParentSession != "" {
		if pst, perr := e.Sessions.State(spec.ParentSession); perr == nil && pst != nil {
			if t := clampTranscript(renderParentTranscript(pst.Snapshot().Messages)); t != "" {
				_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
					Type: sessionstore.EventSystemMessage, SessionID: subSession, AppID: spec.AppID, UserID: spec.UserID,
					Message: &sessionstore.MessagePayload{Role: "system",
						Parts: textParts("Contexte hérité de la conversation parente :\n\n" + t),
						// Marks the seed so clients can skip it: it restates the
						// parent conversation the user is already looking at, and
						// it is addressed to the model. Matching on the French
						// prefix instead would break on any rewording.
						Extra: map[string]any{"source": "fork_seed"}},
				})
			}
		}
	}

	// Seed the isolated sub-session : read-only context first, then the task.
	if spec.MemorySeed != "" {
		if _, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
			Type: sessionstore.EventSystemMessage, SessionID: subSession, AppID: spec.AppID, UserID: spec.UserID,
			Message: &sessionstore.MessagePayload{Role: "system", Parts: textParts(spec.MemorySeed)},
		}); err != nil {
			res.Error = "seed sub-session: " + err.Error()
			return res, fmt.Errorf("runtime: sub-agent %q seed: %w", runID, err)
		}
	}
	if _, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type: sessionstore.EventUserMessage, SessionID: subSession, AppID: spec.AppID, UserID: spec.UserID,
		Message: &sessionstore.MessagePayload{Role: "user", Parts: textParts(spec.Task)},
	}); err != nil {
		res.Error = "seed task: " + err.Error()
		return res, fmt.Errorf("runtime: sub-agent %q task: %w", runID, err)
	}

	// Run the isolated sub-turn off the user-turn pool.
	_, runErr := e.Run(ctx, TurnInput{
		AppID:      spec.AppID,
		SessionID:  subSession,
		UserID:     spec.UserID,
		UserJWT:    spec.UserJWT,
		AgentID:    spec.AgentID,
		AgentRunID: runID,
		SubAgent:   true,
	})
	res.DurationMs = time.Since(start).Milliseconds()
	if runErr != nil {
		res.Error = runErr.Error()
		return res, runErr
	}

	// Project the outcome from the isolated sub-session.
	st, err := e.Sessions.State(subSession)
	if err != nil || st == nil {
		res.Error = "read sub-session state"
		return res, fmt.Errorf("runtime: sub-agent %q read state: %w", runID, err)
	}
	snap := st.Snapshot()
	res.Content = lastAssistantText(snap.Messages)
	res.TokensIn = snap.TokensIn
	res.TokensOut = snap.TokensOut
	res.Status = "completed"
	return res, nil
}

// subSessionID namespaces the isolated sub-session under its parent so the
// agent tree is reconstructable, while staying a SEPARATE session id so the
// session store never loads the parent's history into it.
func subSessionID(parent, runID string) string {
	if parent == "" {
		return "agent::" + runID
	}
	return parent + "::agent::" + runID
}

const maxForkSeedBytes = 100_000 // ~25k tokens : borne le coût du seed fork

// renderParentTranscript aplatit la conversation parent en texte simple pour
// le seed d'un fork (option C). Volontairement simple : role + texte.
func renderParentTranscript(msgs []sessionstore.Message) string {
	var b strings.Builder
	for i := range msgs {
		txt := strings.TrimSpace(msgs[i].Content)
		if txt == "" {
			continue
		}
		role := msgs[i].Role
		if role == "" {
			role = "assistant"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(txt)
		b.WriteString("\n\n")
	}
	return b.String()
}

func clampTranscript(s string) string {
	if len(s) <= maxForkSeedBytes {
		return s
	}
	return "…[début de conversation tronqué]…\n\n" + s[len(s)-maxForkSeedBytes:]
}

func textParts(s string) []sessionstore.MessagePart {
	return []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: s}}
}

func lastAssistantText(msgs []sessionstore.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		if msgs[i].Content != "" {
			return msgs[i].Content
		}
		var b strings.Builder
		for _, p := range msgs[i].Parts {
			if p.Type == sessionstore.PartTypeText {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}
