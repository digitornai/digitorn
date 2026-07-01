//go:build live

package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// Live E2E — composer mode + behavioral enforcement against the REAL
// LLM gateway. Gated by `live` + DIGITORN_LIVE_LLM=1 (see
// live_llm_helpers_test.go). Run with :
//
//   go test -tags live ./internal/runtime/ -run TestLiveMode -v
//
// These assert SERVER-AUTHORITATIVE facts (a filtered tool is never
// offered, a behavior block stops execution + leaves the disk
// untouched) — facts that hold regardless of LLM phrasing.
// =====================================================================

// runLiveTurn injects the user message then runs ONE turn with the given mode.
func (f *liveEngineFixture) runLiveTurn(t *testing.T, mode, userMessage string) {
	t.Helper()
	f.injectUser(t, userMessage)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := f.engine.Run(ctx, dgruntime.TurnInput{
		AppID:     "live-app",
		SessionID: "live-sess",
		UserID:    "test-user",
		UserJWT:   f.userJWT,
		Mode:      mode,
	}); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
}

func behaviorBlockedResult(f *liveEngineFixture, ruleID string) bool {
	for _, ev := range f.session.events {
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil &&
			ev.Tool.Status == "errored" &&
			strings.Contains(ev.Tool.Error, "[BEHAVIOR BLOCKED]") &&
			strings.Contains(ev.Tool.Error, ruleID) {
			return true
		}
	}
	return false
}

// TestLiveMode_ToolGrantsFilterWrite : in a read-only "ask" mode (tool_grants
// = read/ls/grep), filesystem.write is never offered to the model, so it
// cannot be called even when the user explicitly asks to write.
func TestLiveMode_ToolGrantsFilterWrite(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"ask": {
			Label:       "Ask",
			Description: "Read-only",
			ToolGrants: []schema.CapabilityGrant{
				{Module: "filesystem", Tools: []string{"read", "ls", "grep"}},
			},
		},
	}
	f.app.Definition.Runtime.ModesOrder = []string{"ask"}

	f.runLiveTurn(t, "ask", "Create a file called notes.txt containing the text 'hello world'.")

	for _, ev := range f.session.events {
		if ev.Type == sessionstore.EventToolCall && ev.Tool != nil {
			t.Logf("Ask-mode tool_call: %s args=%v", ev.Tool.Name, ev.Tool.Arguments)
		}
	}
	// The filtered tool must never have been called …
	assertToolNotCalled(t, f, "filesystem.write")
	// … and nothing must have been written to disk.
	if _, err := os.Stat(filepath.Join(f.workspace, "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("notes.txt must NOT exist in read-only Ask mode (stat err=%v)", err)
	}
	t.Logf("Ask-mode reply: %s", finalAssistantText(f))
}

// TestLiveMode_SwitchAcrossTurnsEnablesWrite : the same session, two turns,
// two modes. Turn 1 in read-only Ask mode cannot create the file ; switching
// to Build mode in turn 2 makes filesystem.write available and the real model
// creates it. Proves mode switching genuinely toggles tool availability live.
func TestLiveMode_SwitchAcrossTurnsEnablesWrite(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"ask": {Label: "Ask", ToolGrants: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read", "ls", "grep"}},
		}},
		"build": {Label: "Build"}, // no tool_grants → inherits all tools
	}
	f.app.Definition.Runtime.ModesOrder = []string{"ask", "build"}

	// Turn 1 — Ask : write is not offered, so the file cannot be created.
	f.runLiveTurn(t, "ask", "Create a file report.txt containing exactly: STATUS OK")
	if _, err := os.Stat(filepath.Join(f.workspace, "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("report.txt must NOT exist after the read-only Ask turn (stat err=%v)", err)
	}

	// Turn 2 — Build : write becomes available ; the model creates the file.
	f.runLiveTurn(t, "build", "Now create report.txt with exactly the text: STATUS OK")
	assertToolCalled(t, f, "filesystem.write")
	data, err := os.ReadFile(filepath.Join(f.workspace, "report.txt"))
	if err != nil {
		t.Fatalf("report.txt must exist after switching to Build mode: %v", err)
	}
	if !strings.Contains(strings.ToUpper(string(data)), "STATUS OK") {
		t.Errorf("report.txt content = %q, want it to contain 'STATUS OK'", string(data))
	}
	t.Logf("Build-mode created report.txt: %q", string(data))
}

// TestLiveBehavior_BlockRulePreventsWrite : a custom behavior rule
// (action:block on filesystem.write) prevents the write end-to-end. The
// enforced-rules section reaches the system prompt, so a well-behaved model
// usually declines to even attempt the write ; should it try anyway, the
// dispatch-time block (proven deterministically in the unit tests) rejects it.
// Either way the deterministic outcome is the same : no file is ever written.
func TestLiveBehavior_BlockRulePreventsWrite(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Security = &schema.SecurityBlock{
		Behavior: &schema.BehaviorConfig{
			RuleDefinitions: []schema.BehaviorRuleDefinition{{
				ID:      "no_writes",
				When:    schema.RuleWhenPreTool,
				Action:  schema.RuleActionBlock,
				Trigger: []string{"filesystem.write"},
				Message: "File writes are disabled in this app. Tell the user you cannot write files.",
			}},
		},
	}

	f.runLiveTurn(t, "", "Create a file called notes.txt containing the text 'hello world'.")

	// Deterministic core : the file is never created, whichever layer stopped
	// the write (prompt deterrence or dispatch block).
	if _, err := os.Stat(filepath.Join(f.workspace, "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("notes.txt must NOT exist — the no_writes rule must prevent the write (stat err=%v)", err)
	}
	// If the model DID attempt the write, the dispatch block must have caught it.
	if toolWasCalled(f, "filesystem.write") && !behaviorBlockedResult(f, "no_writes") {
		t.Errorf("write was attempted but NOT blocked by the no_writes rule")
	}
	// The reply must acknowledge the inability to write.
	assertSemantic(t, f, "cannot", "can't", "unable", "not able", "disabled", "won't", "not allowed")
	t.Logf("blocked-write reply: %s", finalAssistantText(f))
}

func behaviorDirectiveText(f *liveEngineFixture, source string) string {
	for _, ev := range f.session.events {
		if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil && ev.Message.Extra != nil {
			if s, _ := ev.Message.Extra["source"].(string); s == source {
				return ev.Message.Content
			}
		}
	}
	return ""
}

// TestLiveBehavior_WarnDirectiveReachesModel : an always-on warn rule injects
// a [BEHAVIOR WARNING] system directive when the real model calls the tool —
// proving the warn path (not just block) is wired live end-to-end.
func TestLiveBehavior_WarnDirectiveReachesModel(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Security = &schema.SecurityBlock{
		Behavior: &schema.BehaviorConfig{
			RuleDefinitions: []schema.BehaviorRuleDefinition{{
				ID:      "always_warn_on_read",
				When:    schema.RuleWhenPreTool,
				Action:  schema.RuleActionWarn,
				Trigger: []string{"filesystem.read"},
				Message: "Reads are audited in this app.",
			}},
		},
	}
	f.writeWorkspaceFile(t, "data.txt", "the answer is 42")

	f.runLiveTurn(t, "", "Read data.txt and tell me what it contains.")

	assertToolCalled(t, f, "filesystem.read")
	d := behaviorDirectiveText(f, "behavior_enforcement")
	if d == "" || !strings.Contains(d, "[BEHAVIOR WARNING]") || !strings.Contains(d, "always_warn_on_read") {
		t.Errorf("expected a persisted [BEHAVIOR WARNING] always_warn_on_read directive, got: %q", d)
	}
	assertSemantic(t, f, "42")
}

// TestLiveClassifier_RealModelProducesDirective : with classify_turns on, the
// REAL model acts as the classifier for a clearly-complex task and returns a
// parseable classification that becomes a behavior_classifier directive
// reaching the agent. This is the only live proof that the classifier round
// trip (prompt -> model -> JSON parse -> directive) works against a real LLM.
func TestLiveClassifier_RealModelProducesDirective(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Security = &schema.SecurityBlock{
		Behavior: &schema.BehaviorConfig{
			Profile:       "coding",
			ClassifyTurns: true,
		},
	}

	f.runLiveTurn(t, "", "Refactor the entire authentication module across all files, change the database schema, and add a full test suite.")

	d := behaviorDirectiveText(f, "behavior_classifier")
	if d == "" {
		t.Fatal("the real-model classifier produced no directive for a clearly-complex task")
	}
	if !strings.Contains(d, `type="behavior_classifier"`) {
		t.Errorf("classifier directive malformed: %q", d)
	}
	t.Logf("live classifier directive:\n%s", d)
}

// TestLiveBehavior_ProfileViaModeWiresPrompt : a mode's behavior_profile
// activates a profile per turn. We assert the turn runs cleanly end-to-end
// with the swapped profile (coding) and the model still answers — proving the
// per-turn behavior_profile path is live, not just unit-tested.
func TestLiveBehavior_ProfileViaModeWiresPrompt(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Security = &schema.SecurityBlock{
		Behavior: &schema.BehaviorConfig{Profile: ""}, // base = no rules
	}
	f.app.Definition.Runtime.Modes = map[string]schema.ModeDef{
		"build": {Label: "Build", BehaviorProfile: "coding"},
	}
	f.app.Definition.Runtime.ModesOrder = []string{"build"}

	f.writeWorkspaceFile(t, "data.txt", "the answer is 42")
	f.runLiveTurn(t, "build", "Read data.txt and tell me what it says.")

	assertToolCalled(t, f, "filesystem.read")
	assertSemantic(t, f, "42")
}
