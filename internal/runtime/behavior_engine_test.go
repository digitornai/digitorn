package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// MD-BHV — behavioral enforcement applied per-turn through the engine.
//
// Proves end-to-end :
//   - a composer mode's behavior_profile activates rules (doc point 6) :
//     the swapped "coding" profile blocks a destructive shell call that
//     the empty base profile would have allowed ;
//   - a block prevents the tool from executing (synthetic error result) ;
//   - a warn-level rule injects a durable system directive that reaches
//     the LLM's next round.
// =====================================================================

func behaviorApp(appID, baseProfile string, modeProfile string) *appmgr.RuntimeApp {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	app := secApp(appID, caps, nil)
	app.Definition.Security = &schema.SecurityBlock{
		Behavior: &schema.BehaviorConfig{Profile: baseProfile},
	}
	if modeProfile != "" {
		app.Definition.Runtime.Modes = map[string]schema.ModeDef{
			"build": {Label: "Build", BehaviorProfile: modeProfile},
		}
		app.Definition.Runtime.ModesOrder = []string{"build"}
	}
	return app
}

func behaviorResultErrors(sess *projectingSessions) []string {
	var out []string
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil && ev.Tool.Status == "errored" {
			out = append(out, ev.Tool.Error)
		}
	}
	return out
}

// TestBehavior_ModeProfileBlocksDestructive : the mode's behavior_profile
// "coding" enables confirm_destructive (block), which the empty base profile
// does not. The destructive shell call is rejected before execution.
func TestBehavior_ModeProfileBlocksDestructive(t *testing.T) {
	destructive := []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "shell.bash",
			Arguments: map[string]any{"command": "rm -rf /tmp/x"},
		}}},
		{Content: "understood"},
	}

	// With the mode (behavior_profile: coding) → blocked.
	app := behaviorApp("bhv-app", "", "coding")
	sess := newProjectingSessions("bhv-sess")
	lc := &stubLLM{responses: destructive}
	e := newEngine(t, &stubApps{app: app}, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "bhv-app", SessionID: "bhv-sess", UserID: "u", Mode: "build",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var blocked bool
	for _, errMsg := range behaviorResultErrors(sess) {
		if strings.Contains(errMsg, "[BEHAVIOR BLOCKED]") && strings.Contains(errMsg, "confirm_destructive") {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("mode behavior_profile=coding must block the destructive shell call; errors=%v",
			behaviorResultErrors(sess))
	}

	// Control : same destructive call, NO mode, empty base profile → no rule
	// is active, so the call is NOT blocked by behavior.
	appCtl := behaviorApp("bhv-app2", "", "")
	sessCtl := newProjectingSessions("bhv-sess2")
	ectl := newEngine(t, &stubApps{app: appCtl}, sessCtl, &stubLLM{responses: destructive})
	ectl.Context = wiring.New(secStaticActions{all: secUniverse()})
	if _, err := ectl.Run(context.Background(), dgruntime.TurnInput{
		AppID: "bhv-app2", SessionID: "bhv-sess2", UserID: "u",
	}); err != nil {
		t.Fatalf("control Run: %v", err)
	}
	for _, errMsg := range behaviorResultErrors(sessCtl) {
		if strings.Contains(errMsg, "[BEHAVIOR BLOCKED]") {
			t.Errorf("empty base profile must NOT block (proves the swap drove the block); got %q", errMsg)
		}
	}
}

// TestBehavior_ClassifierInjectsDirective : with classify_turns on, a pre-turn
// classifier LLM call runs and its directive reaches the agent's first round.
func TestBehavior_ClassifierInjectsDirective(t *testing.T) {
	app := behaviorApp("bhv-cls", "coding", "")
	app.Definition.Security.Behavior.ClassifyTurns = true

	sess := newProjectingSessions("bhv-cls-sess")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		// Round 0 = the classifier call (consumed by e.LLM inside Classify).
		{Content: `{"complexity":"complex","approach":"plan_and_confirm","risk_level":"high","directives":["Explore before editing"]}`},
		// Round 1 = the agent's actual turn (final answer, no tools).
		{Content: "ok"},
	}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "bhv-cls", SessionID: "bhv-cls-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(lc.allGots) < 2 {
		t.Fatalf("expected a classifier call + an agent round, got %d LLM calls", len(lc.allGots))
	}
	// The agent's round (the last call) must carry the classifier directive.
	var saw bool
	for _, m := range lc.got.Messages {
		if m.Role == "system" && strings.Contains(m.Content, `type="behavior_classifier"`) &&
			strings.Contains(m.Content, "Explore before editing") {
			saw = true
		}
	}
	if !saw {
		t.Error("classifier directive did not reach the agent's turn")
	}

	var durable bool
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil && ev.Message.Extra != nil {
			if s, _ := ev.Message.Extra["source"].(string); s == "behavior_classifier" {
				durable = true
			}
		}
	}
	if !durable {
		t.Error("classifier directive was not persisted with source=behavior_classifier")
	}
}

// TestBehavior_WarnDirectiveReachesLLM : a warn-level rule (read_before_edit
// under the coding profile) injects a durable system directive that the LLM
// sees on the next round.
func TestBehavior_WarnDirectiveReachesLLM(t *testing.T) {
	app := behaviorApp("bhv-warn", "coding", "")
	sess := newProjectingSessions("bhv-warn-sess")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.edit",
			Arguments: map[string]any{"file_path": "/x.go", "old": "a", "new": "b"},
		}}},
		{Content: "done"},
	}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "bhv-warn", SessionID: "bhv-warn-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Round 2 (the final LLM call) must carry the behavior warning as a
	// system message.
	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	var sawWarning bool
	for _, m := range lc.got.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "[BEHAVIOR WARNING]") &&
			strings.Contains(m.Content, "read_before_edit") {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Error("read_before_edit warning did not reach the LLM as a system directive")
	}

	// The directive is also persisted durably as a system_message event with
	// the behavior_enforcement source.
	var durable bool
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil && ev.Message.Extra != nil {
			if s, _ := ev.Message.Extra["source"].(string); s == "behavior_enforcement" {
				durable = true
			}
		}
	}
	if !durable {
		t.Error("behavior directive was not persisted as a durable system_message")
	}
}
