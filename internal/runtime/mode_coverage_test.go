package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func intp(v int) *int           { return &v }
func floatp(v float64) *float64 { return &v }

// TestMode_SwitchAcrossTurnsTogglesTools : the OFFERED tool list really changes
// when the user switches modes across turns. Ask (grants read only) hides
// shell.bash ; switching to Ops (grants read + bash) reveals it. Proves tools
// are genuinely gated per active mode, turn to turn.
func TestMode_SwitchAcrossTurnsTogglesTools(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	app := secApp("switch-app", caps, nil)
	app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"ask": {Label: "Ask", ToolGrants: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}},
		}},
		"ops": {Label: "Ops", ToolGrants: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}},
			{Module: "shell", Tools: []string{"bash"}},
		}},
	}
	app.Definition.Runtime.ModesOrder = []string{"ask", "ops"}

	apps := &stubApps{app: app}
	sess := newProjectingSessions("switch-sess")
	lc := &stubLLM{responses: []*llm.ChatResponse{{Content: "a"}, {Content: "b"}}}
	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	// Turn 1 : Ask.
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "switch-app", SessionID: "switch-sess", UserID: "u", Mode: "ask",
	}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	ask := offeredSet(lc.allGots[0].Tools)
	if !hasTool(ask, "filesystem.read") {
		t.Errorf("Ask must offer filesystem.read: %v", toolNames(ask))
	}
	if hasTool(ask, "shell.bash") {
		t.Errorf("Ask must NOT offer shell.bash: %v", toolNames(ask))
	}

	// Turn 2 : switch to Ops — shell.bash becomes available.
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "switch-app", SessionID: "switch-sess", UserID: "u", Mode: "ops",
	}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	ops := offeredSet(lc.allGots[1].Tools)
	if !hasTool(ops, "shell.bash") {
		t.Errorf("Ops must offer shell.bash after the switch: %v", toolNames(ops))
	}
	if sess.state.ActiveMode != "ops" {
		t.Errorf("ActiveMode = %q, want ops after switch", sess.state.ActiveMode)
	}
}

// TestMode_DispatchGuardPerCall : within one round the guard discriminates
// per call — an allowed tool dispatches, a hallucinated out-of-mode tool is
// rejected with the synthetic "blocked in mode" error.
func TestMode_DispatchGuardPerCall(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	app := secApp("guard-app", caps, nil)
	app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"ask": {Label: "Ask", ToolGrants: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}},
		}},
	}
	app.Definition.Runtime.ModesOrder = []string{"ask"}

	apps := &stubApps{app: app}
	sess := newProjectingSessions("guard-sess")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{
			{ID: "c1", Name: "filesystem.read", Arguments: map[string]any{"path": "/x"}},
			{ID: "c2", Name: "shell.bash", Arguments: map[string]any{"command": "ls"}},
		}},
		{Content: "done"},
	}}
	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "guard-app", SessionID: "guard-sess", UserID: "u", Mode: "ask",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var readBlocked, bashBlocked bool
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type != sessionstore.EventToolResult || ev.Tool == nil {
			continue
		}
		blocked := ev.Tool.Status == "errored" && strings.Contains(ev.Tool.Error, "blocked in mode")
		switch ev.Tool.Name {
		case "filesystem.read":
			readBlocked = blocked
		case "shell.bash":
			bashBlocked = blocked
		}
	}
	if readBlocked {
		t.Error("filesystem.read is in Ask mode — it must NOT be blocked")
	}
	if !bashBlocked {
		t.Error("shell.bash is NOT in Ask mode — it must be blocked at dispatch")
	}
}

// TestMode_MaxTurnsCapLimitsLoop : a mode's max_turns narrows the per-turn
// tool loop. With max_turns=1 the loop runs exactly one LLM round even though
// the model keeps emitting tool calls ; the control (no cap) runs two.
func TestMode_MaxTurnsCapLimitsLoop(t *testing.T) {
	mkApp := func(withCap bool) *schema.AppDefinition {
		caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
		app := secApp("cap-app", caps, nil)
		md := schema.ModeDef{Label: "One"}
		if withCap {
			md.MaxTurns = intp(1)
		}
		app.Definition.Runtime.Modes = map[string]schema.ModeDef{"one": md}
		app.Definition.Runtime.ModesOrder = []string{"one"}
		return app.Definition
	}

	run := func(def *schema.AppDefinition) int {
		app := secApp("cap-app", nil, nil)
		app.Definition = def
		apps := &stubApps{app: app}
		sess := newProjectingSessions("cap-sess")
		// resp carries a tool call ; the stub returns it on round 1 and a
		// terminal (no tools) on every later round.
		lc := &stubLLM{resp: &llm.ChatResponse{
			ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.read", Arguments: map[string]any{"path": "/x"}}},
		}}
		e := newEngine(t, apps, sess, lc)
		e.Context = wiring.New(secStaticActions{all: secUniverse()})
		if _, err := e.Run(context.Background(), dgruntime.TurnInput{
			AppID: app.Meta.AppID, SessionID: "cap-sess", UserID: "u", Mode: "one",
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return lc.calls
	}

	if got := run(mkApp(true)); got != 1 {
		t.Errorf("with max_turns=1 the loop must make exactly 1 LLM call, got %d", got)
	}
	if got := run(mkApp(false)); got < 2 {
		t.Errorf("control (no cap) must make >=2 LLM calls, got %d", got)
	}
}

// TestMode_TimeoutCapCancelsTurn : a mode's timeout bounds the whole turn.
// A 50ms cap against a 400ms LLM call cancels the turn with a deadline error.
func TestMode_TimeoutCapCancelsTurn(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	app := secApp("to-app", caps, nil)
	app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"slow": {Label: "Slow", Timeout: floatp(0.05)},
	}
	app.Definition.Runtime.ModesOrder = []string{"slow"}

	apps := &stubApps{app: app}
	sess := newProjectingSessions("to-sess")
	// A ctx-aware LLM : it honours cancellation, like the real client. The
	// 50ms mode cap fires before its 400ms "response", so Chat returns the
	// deadline error and the turn fails.
	e := newEngine(t, apps, sess, ctxAwareLLM{delay: 400 * time.Millisecond})
	e.Context = wiring.New(secStaticActions{all: secUniverse()})

	_, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "to-app", SessionID: "to-sess", UserID: "u", Mode: "slow",
	})
	if err == nil {
		t.Fatal("turn must fail when the mode timeout cap is exceeded")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "cancel") {
		t.Errorf("error should reflect the timeout, got: %v", err)
	}
}

// ctxAwareLLM is a minimal LLMChat that respects context cancellation, used to
// prove the mode timeout cap actually bounds the turn.
type ctxAwareLLM struct{ delay time.Duration }

func (c ctxAwareLLM) Chat(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	select {
	case <-time.After(c.delay):
		return &llm.ChatResponse{Content: "late", Model: "slow"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func offeredSet(tools []llm.ToolSpec) map[string]bool {
	seen := map[string]bool{}
	for _, ts := range tools {
		seen[ts.Name] = true
	}
	return seen
}
