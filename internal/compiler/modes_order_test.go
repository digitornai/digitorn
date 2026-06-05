package compiler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureModesOrder : the compiler records runtime.modes keys in YAML
// insertion order (Go maps lose it), which the mode default-policy needs.
func TestCaptureModesOrder(t *testing.T) {
	const appYAML = `schema_version: 2
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
  modes:
    ask:
      label: Ask
    plan:
      label: Plan
    build:
      label: Build
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(appYAML), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}

	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if res.Definition == nil || res.Definition.Runtime == nil {
		t.Fatal("no runtime block compiled")
	}
	got := res.Definition.Runtime.ModesOrder
	want := []string{"ask", "plan", "build"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ModesOrder = %v, want %v (YAML insertion order)", got, want)
	}
	// The map must still carry all three definitions.
	if len(res.Definition.Runtime.Modes) != 3 {
		t.Errorf("Modes count = %d, want 3", len(res.Definition.Runtime.Modes))
	}
}
