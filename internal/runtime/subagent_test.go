package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// multiProjecting is a session-store stub keyed by session id : each session
// gets its own projected SessionState. Unlike projectingSessions (single
// state), it lets a test prove cross-session isolation — a sub-agent's
// sub-session never shares the parent's state.
type multiProjecting struct {
	mu     sync.Mutex
	states map[string]*sessionstore.SessionState
	seqs   map[string]uint64
}

func newMultiProjecting() *multiProjecting {
	return &multiProjecting{
		states: map[string]*sessionstore.SessionState{},
		seqs:   map[string]uint64{},
	}
}

func (m *multiProjecting) stateLocked(sid string) *sessionstore.SessionState {
	st := m.states[sid]
	if st == nil {
		st = sessionstore.NewSessionState(sid)
		m.states[sid] = st
	}
	return st
}

func (m *multiProjecting) State(sid string) (*sessionstore.SessionState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked(sid), nil
}

func (m *multiProjecting) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seqs[ev.SessionID]++
	ev.Seq = m.seqs[ev.SessionID]
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	sessionstore.Apply(m.stateLocked(ev.SessionID), &ev)
	return ev.Seq, nil
}

func (m *multiProjecting) Append(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	return m.AppendDurable(ctx, ev)
}

// TestRunSubAgent_IsolatedFromParent : the headline property. A sub-agent runs
// in a fresh sub-session seeded only with the memory seed + task ; it must
// NEVER see the parent's conversation history.
func TestRunSubAgent_IsolatedFromParent(t *testing.T) {
	app := secApp("ma-app", &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}, nil)
	sess := newMultiProjecting()

	// Parent session carries a distinctive secret the sub-agent must not see.
	if _, err := sess.AppendDurable(context.Background(), sessionstore.Event{
		Type: sessionstore.EventUserMessage, SessionID: "parent-sess", AppID: "ma-app", UserID: "u",
		Message: &sessionstore.MessagePayload{Role: "user", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: "PARENT_SECRET_HISTORY_42"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "SUBRESULT", Model: "m",
		Usage: llm.Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9}}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	res, err := e.RunSubAgent(context.Background(), dgruntime.SubAgentSpec{
		AppID:         "ma-app",
		ParentSession: "parent-sess",
		UserID:        "u",
		AgentID:       "main",
		Task:          "summarize the project",
		MemorySeed:    "goal: ship the multi-agent system",
	})
	if err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	// Result shape.
	if res.Status != "completed" {
		t.Errorf("status = %q, want completed (err=%q)", res.Status, res.Error)
	}
	if res.Content != "SUBRESULT" {
		t.Errorf("content = %q, want SUBRESULT", res.Content)
	}
	if res.AgentID != "main" {
		t.Errorf("agent id = %q, want main", res.AgentID)
	}
	if !strings.HasPrefix(res.RunID, "main#") {
		t.Errorf("run id = %q, want main#... prefix", res.RunID)
	}
	if res.Session != "parent-sess::agent::"+res.RunID {
		t.Errorf("session = %q, want isolated sub-session under the parent", res.Session)
	}

	// ISOLATION : the LLM the sub-agent invoked saw the task + seed, never the
	// parent's secret history.
	if lc.got == nil {
		t.Fatal("LLM not called for the sub-agent")
	}
	var sawTask, sawSeed, leaked bool
	for _, mm := range lc.got.Messages {
		text := mm.Content
		for _, p := range mm.Parts {
			text += p.Text
		}
		if strings.Contains(text, "summarize the project") {
			sawTask = true
		}
		if strings.Contains(text, "ship the multi-agent system") {
			sawSeed = true
		}
		if strings.Contains(text, "PARENT_SECRET_HISTORY_42") {
			leaked = true
		}
	}
	if leaked {
		t.Error("ISOLATION BREACH: the sub-agent saw the parent's history")
	}
	if !sawTask {
		t.Error("sub-agent must see its task")
	}
	if !sawSeed {
		t.Error("sub-agent must see its read-only memory seed")
	}

	// The parent session must be untouched by the sub-agent run.
	pst, _ := sess.State("parent-sess")
	for _, msg := range pst.Snapshot().Messages {
		if msg.Role == "assistant" && msg.Content == "SUBRESULT" {
			t.Error("sub-agent result leaked into the parent session")
		}
	}
}

// TestRunSubAgent_AgentSelectionAndRunID : an explicit RunID is honoured and the
// target agent is the one that runs.
func TestRunSubAgent_AgentSelectionAndRunID(t *testing.T) {
	app := secApp("ma-app2", &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}, nil)
	app.Definition.Agents = append(app.Definition.Agents, schema.Agent{
		ID: "researcher", Role: "specialist",
		Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
		SystemPrompt: "research",
	})
	sess := newMultiProjecting()
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done", Model: "m"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	res, err := e.RunSubAgent(context.Background(), dgruntime.SubAgentSpec{
		AppID: "ma-app2", ParentSession: "p", UserID: "u",
		AgentID: "researcher", RunID: "researcher#fixed01", Task: "go",
	})
	if err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	if res.RunID != "researcher#fixed01" {
		t.Errorf("explicit run id must be kept, got %q", res.RunID)
	}
	if lc.got.AgentID != "researcher#fixed01" {
		t.Errorf("the distinct run id must reach the LLM, got %q", lc.got.AgentID)
	}
}
