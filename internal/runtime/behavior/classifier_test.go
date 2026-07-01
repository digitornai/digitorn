package behavior

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func TestClassifier_ShouldSkipFollowup(t *testing.T) {
	yes := []string{"ok", "yes", "yes!", "continue", "go ahead", "merci", "👍", "lgtm"}
	for _, s := range yes {
		if !shouldSkipFollowup(s) {
			t.Errorf("%q should be a skippable follow-up", s)
		}
	}
	no := []string{"please refactor the auth module", "what does this function do?",
		strings.Repeat("ok ", 30)}
	for _, s := range no {
		if shouldSkipFollowup(s) {
			t.Errorf("%q should NOT be skippable", s)
		}
	}
}

func TestClassifier_ShouldRunThisTurn(t *testing.T) {
	first := map[string]any{"frequency": "first_turn"}
	if !shouldRunThisTurn(0, first, "do X") || shouldRunThisTurn(1, first, "do X") {
		t.Error("first_turn must run only on turn 0")
	}
	everyN := map[string]any{"frequency": "every_n_turns", "frequency_n": 3}
	if !shouldRunThisTurn(0, everyN, "x") || shouldRunThisTurn(1, everyN, "x") || !shouldRunThisTurn(3, everyN, "x") {
		t.Error("every_n_turns=3 must run on turns 0 and 3, not 1")
	}
	every := map[string]any{"frequency": "every_turn"}
	if !shouldRunThisTurn(5, every, "x") {
		t.Error("every_turn must always run")
	}
	// skip_followups gates before frequency.
	if shouldRunThisTurn(0, map[string]any{"frequency": "every_turn", "skip_followups": true}, "ok") {
		t.Error("a follow-up must be skipped even on every_turn")
	}
}

func TestClassifier_ParseClassification(t *testing.T) {
	direct := `{"complexity":"simple","approach":"direct","risk_level":"low","directives":["go"]}`
	if got := parseClassification(direct); got == nil || got["approach"] != "direct" {
		t.Fatalf("direct JSON parse failed: %v", got)
	}
	fenced := "```json\n{\"complexity\":\"simple\",\"directives\":[\"go\"]}\n```"
	if got := parseClassification(fenced); got == nil {
		t.Fatal("fenced JSON parse failed")
	}
	embedded := "Sure! Here:\n{\"directives\":[\"go\"]}\nThanks"
	if got := parseClassification(embedded); got == nil {
		t.Fatal("embedded JSON parse failed")
	}
	// skip_reason with no directives → nil (skip).
	if got := parseClassification(`{"skip_reason":"trivial"}`); got != nil {
		t.Errorf("skip_reason with no directives must yield nil, got %v", got)
	}
	if got := parseClassification("not json at all"); got != nil {
		t.Errorf("garbage must yield nil, got %v", got)
	}
}

func TestClassifier_FormatDirectiveMessage(t *testing.T) {
	cfg := map[string]any{}
	cls := map[string]any{
		"complexity": "complex", "approach": "plan_and_confirm",
		"risk_level": "high",
		"directives": []any{"Explore the module first", "Write a numbered plan"},
	}
	out := formatDirectiveMessage(cfg, cls)
	for _, want := range []string{
		`type="behavior_classifier"`, `complexity="complex"`, `risk="high"`,
		"Explore the module first", "2. Write a numbered plan",
		"Plan and get user approval", // approach label resolved from defaults
		"</digitorn-directive>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("directive missing %q:\n%s", want, out)
		}
	}
	// high risk (>= medium threshold) injects the warning.
	if !strings.Contains(out, "Confirm destructive") {
		t.Error("high risk must inject the high_risk_warning")
	}
	// No directives → empty.
	if formatDirectiveMessage(cfg, map[string]any{"directives": []any{}}) != "" {
		t.Error("no directives must produce empty output")
	}
}

func TestClassifier_BuildSystemPrompt(t *testing.T) {
	p := buildSystemPrompt(nil)
	for _, want := range []string{`"trivial"`, `"plan_and_confirm"`, `"high"`, "Complexity levels", "Risk assessment"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// Custom system_prompt overrides entirely.
	if got := buildSystemPrompt(map[string]any{"system_prompt": "CUSTOM"}); got != "CUSTOM" {
		t.Errorf("custom system_prompt must override, got %q", got)
	}
}

func TestEngine_ClassifyProducesDirectiveAndGating(t *testing.T) {
	enabled := true
	be := New(&schema.BehaviorConfig{
		Profile:       "coding",
		ClassifyTurns: true,
		Classifier:    &schema.ClassifierConfig{SkipFollowups: &enabled},
	})
	const sid = "s1"
	be.OnTurnStart(sid)

	var calls int
	chat := func(_ context.Context, system, user string) (string, error) {
		calls++
		if !strings.Contains(user, "do a big refactor") {
			t.Errorf("user prompt should carry the user message; got %q", user)
		}
		return `{"complexity":"complex","approach":"delegate","risk_level":"medium","directives":["Delegate exploration"]}`, nil
	}

	out := be.Classify(context.Background(), sid, ClassifyInput{UserMessage: "do a big refactor"}, chat)
	if !strings.Contains(out, "Delegate exploration") {
		t.Fatalf("classify must return the directive; got %q", out)
	}
	if calls != 1 {
		t.Fatalf("chat must be called once, got %d", calls)
	}

	// A follow-up is skipped without calling the LLM.
	calls = 0
	if out := be.Classify(context.Background(), sid, ClassifyInput{UserMessage: "ok"}, chat); out != "" {
		t.Errorf("follow-up must skip, got %q", out)
	}
	if calls != 0 {
		t.Errorf("follow-up must NOT call the LLM, got %d calls", calls)
	}
}

func TestEngine_ClassifyDisabledIsInert(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"}) // classify_turns off
	if be.ClassifyEnabled() {
		t.Fatal("classify must be disabled by default")
	}
	called := false
	out := be.Classify(context.Background(), "s1", ClassifyInput{UserMessage: "x"},
		func(context.Context, string, string) (string, error) { called = true; return "{}", nil })
	if out != "" || called {
		t.Errorf("disabled classify must be inert (out=%q called=%v)", out, called)
	}
}
