package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
)

// composerModeApp builds a minimal valid manifest, splicing the caller's
// runtime.modes block in. Everything else is fixed boilerplate.
func composerModeApp(modesBlock string) string {
	return `schema_version: 2
app:
  app_id: modeapp
  name: ModeApp
  version: "0.1.0"
agents:
  - id: main
    role: worker
    brain:
      provider: anthropic
      model: claude-sonnet-4-6
      config:
        api_key: "sk-ant-test"
    system_prompt: "hi"
    modules:
      - filesystem
tools:
  modules:
    filesystem:
      config:
        workspace: "."
  capabilities:
    default_policy: auto
runtime:
` + modesBlock
}

func compileModes(t *testing.T, modesBlock string) *diagnostic.Bag {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"),
		[]byte(composerModeApp(modesBlock)), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return res.Diagnostics
}

func hasCode(diags []diagnostic.Diagnostic, code diagnostic.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestComposerModes_BoundsAreErrors : max_turns < 1 and timeout <= 0 are hard
// constraints (the JSON schema sets minimum:1 / exclusiveMinimum:0).
func TestComposerModes_BoundsAreErrors(t *testing.T) {
	bag := compileModes(t, `  modes:
    bad:
      label: Bad
      max_turns: 0
      timeout: 0
`)
	errs := bag.Errors()
	if !hasCode(errs, diagnostic.CodeOutOfRange) {
		t.Fatalf("expected DGT-E0103 (out of range) for max_turns:0 / timeout:0, got:\n%s",
			formatDiags(bag))
	}
	// Both fields are out of range → two distinct diagnostics.
	n := 0
	for _, d := range errs {
		if d.Code == diagnostic.CodeOutOfRange {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expected 2 out-of-range errors (max_turns + timeout), got %d:\n%s",
			n, formatDiags(bag))
	}
}

// TestComposerModes_UnknownIconAccentAreWarnings : icon / accent are UI hints ;
// unknown values warn (the client falls back) but the app still compiles.
func TestComposerModes_UnknownIconAccentAreWarnings(t *testing.T) {
	bag := compileModes(t, `  modes:
    ask:
      label: Ask
      icon: rocket
      accent: chartreuse
`)
	if bag.HasErrors() {
		t.Fatalf("unknown icon/accent must NOT be errors, got:\n%s", formatDiags(bag))
	}
	if !hasCode(bag.Warnings(), diagnostic.CodeUnknownEnumHint) {
		t.Fatalf("expected DGT-W0007 warnings for unknown icon/accent, got:\n%s",
			formatDiags(bag))
	}
	n := 0
	for _, d := range bag.Warnings() {
		if d.Code == diagnostic.CodeUnknownEnumHint {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expected 2 enum-hint warnings (icon + accent), got %d:\n%s",
			n, formatDiags(bag))
	}
}

func composerBehaviorApp(behaviorBlock string) string {
	return `schema_version: 2
app:
  app_id: bhvapp
  name: BhvApp
  version: "0.1.0"
agents:
  - id: main
    role: worker
    brain:
      provider: anthropic
      model: claude-sonnet-4-6
      config:
        api_key: "sk-ant-test"
    system_prompt: "hi"
    modules:
      - filesystem
tools:
  modules:
    filesystem:
      config:
        workspace: "."
  capabilities:
    default_policy: auto
security:
` + behaviorBlock
}

func compileBehavior(t *testing.T, behaviorBlock string) *diagnostic.Bag {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"),
		[]byte(composerBehaviorApp(behaviorBlock)), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return res.Diagnostics
}

// TestBehavior_RuleEnumsAndClassifierBounds : invalid rule when/action and
// classifier frequency are errors ; bad numeric bounds are errors.
func TestBehavior_RuleEnumsAndClassifierBounds(t *testing.T) {
	bag := compileBehavior(t, `  behavior:
    profile: coding
    rule_definitions:
      - id: r1
        when: sometimes
        action: nuke
        message: bad
    classify_turns: true
    classifier:
      frequency: hourly
      frequency_n: -1
      timeout: -5
      max_directives: -2
`)
	if !hasCode(bag.Errors(), diagnostic.CodeBadEnum) {
		t.Errorf("invalid when/action/frequency must be DGT-E0104 errors:\n%s", formatDiags(bag))
	}
	if !hasCode(bag.Errors(), diagnostic.CodeOutOfRange) {
		t.Errorf("negative frequency_n / timeout / max_directives must be DGT-E0103 errors:\n%s", formatDiags(bag))
	}
}

// TestBehavior_UnknownProfileWarns : an unknown profile warns (resolves to no
// rules) but still compiles.
func TestBehavior_UnknownProfileWarns(t *testing.T) {
	bag := compileBehavior(t, `  behavior:
    profile: ninja
`)
	if bag.HasErrors() {
		t.Fatalf("unknown profile must not be an error:\n%s", formatDiags(bag))
	}
	if !hasCode(bag.Warnings(), diagnostic.CodeUnknownEnumHint) {
		t.Errorf("unknown profile must warn (DGT-W0007):\n%s", formatDiags(bag))
	}
}

// TestBehavior_ValidConfigClean : a documented profile + valid rules + valid
// classifier compile without behavior diagnostics.
func TestBehavior_ValidConfigClean(t *testing.T) {
	bag := compileBehavior(t, `  behavior:
    profile: dev
    rule_definitions:
      - id: r1
        when: pre_tool
        action: warn
        trigger: [filesystem.edit]
        message: read first
    classify_turns: true
    classifier:
      frequency: every_n_turns
      frequency_n: 3
      timeout: 15
      max_directives: 5
`)
	if bag.HasErrors() {
		t.Fatalf("valid behavior config produced errors:\n%s", formatDiags(bag))
	}
	if hasCode(bag.Warnings(), diagnostic.CodeUnknownEnumHint) {
		t.Errorf("documented profile must not warn:\n%s", formatDiags(bag))
	}
}

// TestComposerModes_KnownValuesClean : documented icon/accent + valid bounds
// compile without any mode diagnostics.
func TestComposerModes_KnownValuesClean(t *testing.T) {
	bag := compileModes(t, `  modes:
    ask:
      label: Ask
      icon: lightbulb
      accent: cyan
      max_turns: 8
      timeout: 60
    plan:
      label: Plan
      icon: map
      accent: purple
`)
	if bag.HasErrors() {
		t.Fatalf("valid composer modes produced errors:\n%s", formatDiags(bag))
	}
	if hasCode(bag.Warnings(), diagnostic.CodeUnknownEnumHint) {
		t.Errorf("documented icon/accent must not warn:\n%s", formatDiags(bag))
	}
}
