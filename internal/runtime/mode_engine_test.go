package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/wiring"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// MD-3 — composer mode applied in the engine (the security core).
//
// Proves, end-to-end through the real engine + context builder :
//   - the offered tool list sent to the LLM is filtered to the mode's
//     tool_grants allow-list ;
//   - a hallucinated call to a blocked tool is rejected at dispatch with
//     the synthetic "blocked in mode" error (defense in depth) ;
//   - a durable mode-switch system directive reaches the LLM context ;
//   - the session's ActiveMode is bound (sticky for later turns).
// =====================================================================

func modeApp() *appmgr.RuntimeApp {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	app := secApp("mode-app", caps, nil)
	app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"ask": {
			Label:        "Ask",
			Description:  "Read-only Q&A",
			SystemPrompt: "Mode: Ask. Read-only investigation.",
			ToolGrants: []schema.CapabilityGrant{
				{Module: "filesystem", Tools: []string{"read"}},
			},
		},
	}
	app.Definition.Runtime.ModesOrder = []string{"ask"}
	return app
}

func TestMode_FiltersToolsGuardsDispatchAndAnnounces(t *testing.T) {
	app := modeApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("mode-sess")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		// Round 1 : the model hallucinates a blocked tool (filesystem.delete
		// is not in Ask mode's allow-list).
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.delete",
			Arguments: map[string]any{"path": "/x"},
		}}},
		{Content: "ok, blocked"},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mode-app", SessionID: "mode-sess", UserID: "u", Mode: "ask",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// (1) Tool list filtered : the LLM saw filesystem.read but NOT the
	// blocked domain tools. Meta-tools are always available.
	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	seen := map[string]bool{}
	for _, ts := range lc.got.Tools {
		seen[ts.Name] = true
	}
	if !hasTool(seen, "filesystem.read") {
		t.Errorf("Ask mode must keep filesystem.read : %v", toolNames(seen))
	}
	for _, blocked := range []string{"filesystem.delete", "shell.bash", "http.get"} {
		if hasTool(seen, blocked) {
			t.Errorf("Ask mode must filter %q from the LLM tool list : %v", blocked, toolNames(seen))
		}
	}
	if !hasTool(seen, "context_builder.run_parallel") {
		t.Errorf("execution primitives must remain available when the agent has tools : %v", toolNames(seen))
	}

	// (2) Switch directive reached the LLM context.
	var sawDirective bool
	for _, m := range lc.got.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "[Mode: Ask]") &&
			strings.Contains(m.Content, "blocked in this mode") {
			sawDirective = true
		}
	}
	if !sawDirective {
		t.Error("mode-switch directive ([Mode: Ask] + blocked list) did not reach the LLM")
	}

	// (3) Dispatch guard : the hallucinated filesystem.delete was rejected.
	var blockedResult bool
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil &&
			ev.Tool.Status == "errored" && strings.Contains(ev.Tool.Error, "blocked in mode") {
			blockedResult = true
		}
	}
	if !blockedResult {
		t.Error("hallucinated filesystem.delete was not rejected by the mode guard")
	}

	// (4) Session bound to the mode (sticky).
	if got := sess.state.ActiveMode; got != "ask" {
		t.Errorf("session ActiveMode = %q, want ask", got)
	}
}

// TestMode_StickyAcrossTurns : a second turn that omits the mode reuses the
// session's bound mode (the doc's "picker restores last mode", made
// server-authoritative). The directive is NOT re-emitted when unchanged.
func TestMode_StickyAcrossTurns(t *testing.T) {
	app := modeApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("mode-sess")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}

	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	// Turn 1 : pick Ask.
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mode-app", SessionID: "mode-sess", UserID: "u", Mode: "ask",
	}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	directivesAfter1 := countSystemDirectives(sess, "mode_switch")

	// Turn 2 : omit the mode → sticky Ask, no new directive.
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mode-app", SessionID: "mode-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if sess.state.ActiveMode != "ask" {
		t.Errorf("ActiveMode = %q, want ask (sticky)", sess.state.ActiveMode)
	}
	if got := countSystemDirectives(sess, "mode_switch"); got != directivesAfter1 {
		t.Errorf("mode-switch directives = %d, want %d (unchanged mode must not re-announce)", got, directivesAfter1)
	}
	// The Ask filter still applies on the sticky turn.
	seen := map[string]bool{}
	for _, ts := range lc.got.Tools {
		seen[ts.Name] = true
	}
	if hasTool(seen, "shell.bash") {
		t.Errorf("sticky Ask mode must still filter shell.bash : %v", toolNames(seen))
	}
}

func countSystemDirectives(sess *projectingSessions, source string) int {
	n := 0
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil && ev.Message.Extra != nil {
			if s, _ := ev.Message.Extra["source"].(string); s == source {
				n++
			}
		}
	}
	return n
}

// hasTool matches a canonical FQN against the sanitized form the LLM sees.
func hasTool(seen map[string]bool, fqn string) bool {
	if seen[fqn] {
		return true
	}
	if i := strings.Index(fqn, "."); i != -1 {
		return seen[fqn[:i]+"__"+fqn[i+1:]]
	}
	return false
}

func toolNames(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}
