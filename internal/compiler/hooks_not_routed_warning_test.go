package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/pkg/module"

	_ "github.com/digitornai/digitorn/internal/modules/filesystem"
)

// notRoutedApp declares two hooks : one on a ROUTED event (session_start,
// which the runtime now emits) and one on a NOT-YET-ROUTED event
// (agent_spawn, gated behind unimplemented multi-agent). Both are valid
// events so neither is an error ; only the not-routed one must warn.
const notRoutedApp = `schema_version: 2

app:
  app_id: hooks-not-routed
  name: Hooks Not Routed
  version: "1.0.0"

runtime:
  hooks:
    - id: routed_ok
      "on": session_start
      condition: { type: always }
      action: { type: log, message: "session up" }
    - id: dead_hook
      "on": agent_spawn
      condition: { type: always }
      action: { type: log, message: "spawned" }

agents:
  - id: main
    role: assistant
    brain:
      provider: openai
      model: gpt-4o-mini
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "Test agent."
    modules: [filesystem]

tools:
  modules:
    filesystem:
      config: { workspace: "." }
  capabilities:
    default_policy: auto
    grant:
      - module: filesystem
        tools: [read, write, glob, grep]
`

// TestHooks_NotRoutedEventWarnsButCompiles : a hook on a not-yet-routed
// event compiles (no error) but produces exactly one DGT-W0006 warning,
// so an author is never surprised by a silently-dead hook. A routed
// event (session_start) produces NO such warning.
func TestHooks_NotRoutedEventWarnsButCompiles(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "hooks-not-routed")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app.yaml"), []byte(notRoutedApp), 0o644); err != nil {
		t.Fatal(err)
	}

	c := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})
	res, err := c.Compile(appDir)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}

	// Not an error : the app still compiles.
	if errs := res.Diagnostics.Errors(); len(errs) != 0 {
		for _, d := range errs {
			t.Errorf("unexpected error [%s] %s", d.Code, d.Message)
		}
		t.Fatal("not-routed event must NOT be a compile error")
	}
	if !res.OK() {
		t.Fatal("res.OK() false despite zero errors")
	}

	// Exactly one not-routed warning, for the agent_spawn hook only.
	var routedWarns int
	for _, w := range res.Diagnostics.Warnings() {
		if w.Code == diagnostic.CodeHookEventNotRouted {
			routedWarns++
		}
	}
	if routedWarns != 1 {
		t.Errorf("expected exactly 1 DGT-W0006 (agent_spawn) warning, got %d", routedWarns)
	}
}
