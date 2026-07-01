package runtime_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
)

// TestTurn_LLMRequestCarriesIdentity : every LLM request is attributed to the
// session, the user, and the running agent so the gateway/provider can trace
// it (Paul's requirement). For a top-level turn the agent id is the entry
// agent's logical id.
func TestTurn_LLMRequestCarriesIdentity(t *testing.T) {
	app := secApp("ident-app", &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}, nil)
	sess := newProjectingSessions("ident-sess")
	seedUser(t, sess, "ident-app", "ident-sess", "hello")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "hi"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "ident-app", SessionID: "ident-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	if lc.got.SessionID != "ident-sess" {
		t.Errorf("SessionID = %q, want ident-sess", lc.got.SessionID)
	}
	if lc.got.UserID != "u" {
		t.Errorf("UserID = %q, want u", lc.got.UserID)
	}
	if lc.got.AgentID != "main" {
		t.Errorf("AgentID = %q, want main (entry agent)", lc.got.AgentID)
	}
}

// TestTurn_EntryAgentSelected : with two agents and runtime.entry_agent set to
// the second, the turn runs that agent (not the hardcoded first).
func TestTurn_EntryAgentSelected(t *testing.T) {
	app := secApp("entry-app", &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}, nil)
	app.Definition.Agents = append(app.Definition.Agents, schema.Agent{
		ID: "second", Role: "assistant",
		Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
		SystemPrompt: "second",
	})
	app.Definition.Runtime.EntryAgent = "second"

	sess := newProjectingSessions("entry-sess")
	seedUser(t, sess, "entry-app", "entry-sess", "hello")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "hi"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "entry-app", SessionID: "entry-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got.AgentID != "second" {
		t.Errorf("entry_agent must select 'second', got AgentID = %q", lc.got.AgentID)
	}
}

// TestTurn_ExplicitAgentIDWins : an explicit TurnInput.AgentID overrides
// entry_agent (the AgentManager uses this to run a chosen sub-agent), and the
// distinct AgentRunID is what reaches the LLM.
func TestTurn_ExplicitAgentIDWins(t *testing.T) {
	app := secApp("explicit-app", &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}, nil)
	app.Definition.Agents = append(app.Definition.Agents, schema.Agent{
		ID: "second", Role: "assistant",
		Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
		SystemPrompt: "second",
	})
	app.Definition.Runtime.EntryAgent = "second"

	sess := newProjectingSessions("explicit-sess")
	seedUser(t, sess, "explicit-app", "explicit-sess", "hello")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "hi"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "explicit-app", SessionID: "explicit-sess", UserID: "u",
		AgentID: "main", AgentRunID: "main#abc123",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got.AgentID != "main#abc123" {
		t.Errorf("explicit AgentRunID must reach the LLM, got %q", lc.got.AgentID)
	}
}
