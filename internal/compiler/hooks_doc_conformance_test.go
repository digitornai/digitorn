package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler"
	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/pkg/module"

	// Register the filesystem module so the compiler's catalog can
	// resolve the app's `modules: [filesystem]` declaration — mirrors
	// how the daemon wires catalog.RegistrySource at boot.
	_ "github.com/mbathepaul/digitorn/internal/modules/filesystem"
)

// docHooksApp declares ONE hook per documented condition and action,
// in the exact YAML shapes from docs-site/language/31-tool-hooks.md.
// Before the vocabulary reconciliation, most of these failed to compile
// ("unknown hook condition/action") because the compiler's invented
// vocabulary diverged from the documentation. This test is the
// regression lock : every documented hook MUST compile clean.
const docHooksApp = `schema_version: 2

app:
  app_id: hooks-doc-conformance
  name: Hooks Doc Conformance
  version: "1.0.0"

runtime:
  hooks:
    # --- conditions (14) ---
    - id: c_always
      "on": tool_start
      condition: { type: always }
      action: { type: log, message: "x" }
    - id: c_never
      "on": tool_start
      condition: { type: never }
      action: { type: log, message: "x" }
    - id: c_context_pressure
      "on": turn_end
      condition: { type: context_pressure, threshold: 0.8 }
      action: { type: compact_context, strategy: summarize, keep_last: 10 }
    - id: c_turn_count
      "on": turn_end
      condition: { type: turn_count, threshold: 5, every: 5 }
      action: { type: log, message: "x" }
    - id: c_tool_calls
      "on": tool_end
      condition: { type: tool_calls, threshold: 3 }
      action: { type: notify, title: "t", message: "m" }
    - id: c_message_count
      "on": turn_end
      condition: { type: message_count, threshold: 20 }
      action: { type: log, message: "x" }
    - id: c_tool_name
      "on": tool_end
      condition: { type: tool_name, match: "filesystem.write" }
      action: { type: lsp_diagnose, path_field: tool.params.path }
    - id: c_tool_failed
      "on": tool_end
      condition: { type: tool_failed }
      action: { type: notify, level: error, message: "failed {{tool.name}}" }
    - id: c_content_contains
      "on": turn_end
      condition: { type: content_contains, keyword: "rm -rf" }
      action: { type: log, level: warn, message: "danger" }
    - id: c_error_type
      "on": error
      condition: { type: error_type, match: "Timeout.*" }
      action: { type: log, level: error, message: "timeout" }
    - id: c_expression
      "on": turn_end
      condition: { type: expression, expr: "tokens_used > 1000" }
      action: { type: log, message: "x" }
    - id: c_all_of
      "on": tool_start
      condition:
        type: all_of
        conditions:
          - { type: tool_name, match: "filesystem.write" }
          - type: not
            condition: { type: tool_failed }
      action: { type: gate, allow: false, reason: "blocked" }
    - id: c_any_of
      "on": tool_end
      condition:
        type: any_of
        conditions:
          - { type: tool_failed }
          - { type: content_contains, keyword: "error" }
      action: { type: notify, level: error, message: "problem" }

    # --- actions (13 general-purpose) ---
    - id: a_compact_context
      "on": pre_compact
      condition: { type: always }
      action: { type: compact_context, strategy: truncate, keep_last: 30 }
    - id: a_inject_message
      "on": turn_start
      condition: { type: always }
      action: { type: inject_message, content: "remember to test", role: user }
    - id: a_module_action
      "on": tool_end
      condition: { type: tool_name, match: "filesystem.write" }
      action: { type: module_action, module: filesystem, action: read, params: { path: "x" } }
    - id: a_module_action_inject
      "on": tool_end
      condition: { type: tool_name, match: "filesystem.write" }
      action: { type: module_action_inject, action: "filesystem.read", params: { path: "x" }, role: user }
    - id: a_log
      "on": tool_start
      condition: { type: always }
      action: { type: log, message: "{{tool.name}}", level: info }
    - id: a_shell
      "on": tool_end
      condition: { type: tool_name, match: "filesystem.write" }
      action: { type: shell, command: "echo {{tool.params.path}}", on_error: log }
    - id: a_gate
      "on": tool_start
      condition: { type: tool_name, match: "bash.run" }
      action: { type: gate, allow: false, reason: "shell disabled" }
    - id: a_transform_params
      "on": tool_start
      condition: { type: tool_name, match: "filesystem.read" }
      action: { type: transform_params, transformation: { path: "safe.txt" } }
    - id: a_transform_result
      "on": tool_end
      condition: { type: tool_name, match: "filesystem.read" }
      action: { type: transform_result, transformation: { redacted: true } }
    - id: a_chain
      "on": tool_end
      condition: { type: tool_failed }
      action:
        type: chain
        actions:
          - { type: log, level: error, message: "tool failed" }
          - { type: notify, level: error, message: "{{tool.error}}" }
    - id: a_notify
      "on": tool_end
      condition: { type: always }
      action: { type: notify, title: "done", message: "{{tool.name}}", level: info, tag: ops }
    - id: a_pipe
      "on": tool_end
      condition: { type: tool_name, match: "web.fetch" }
      action:
        type: pipe
        to: web.extract
        map: { html: "{{tool.result.text}}" }
        extra: { max_links: 10 }
        on_error: log
    - id: a_lsp_diagnose
      "on": tool_end
      condition: { type: tool_name, match: "filesystem.write" }
      action: { type: lsp_diagnose, path_field: tool.params.path, content_field: tool.params.content, publish: true, inject_result: true }

agents:
  - id: main
    role: assistant
    brain:
      provider: openai
      model: gpt-4o-mini
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "You are a test agent."
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

// TestHooksDocConformance_AllDocumentedHooksCompile compiles an app
// declaring every documented condition and action and asserts ZERO
// error diagnostics. This is the proof that following the hooks
// documentation produces a compilable app — the bug class that the
// vocabulary reconciliation fixed.
func TestHooksDocConformance_AllDocumentedHooksCompile(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "hooks-doc-conformance")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app.yaml"),
		[]byte(docHooksApp), 0o644); err != nil {
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
	errs := res.Diagnostics.Errors()
	if len(errs) != 0 {
		for _, d := range errs {
			t.Errorf("unexpected diagnostic [%s] %s", d.Code, d.Message)
		}
		t.Fatalf("documented hooks app produced %d error(s) — the doc does not compile", len(errs))
	}
	if !res.OK() {
		t.Fatal("res.OK() is false despite zero errors")
	}
}
