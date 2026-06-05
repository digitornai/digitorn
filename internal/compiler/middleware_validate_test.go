package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
)

func composerMiddlewareApp(mwBlock string) string {
	return `schema_version: 2
app:
  app_id: mwapp
  name: MwApp
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
` + mwBlock
}

func compileMiddleware(t *testing.T, mwBlock string) *diagnostic.Bag {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"),
		[]byte(composerMiddlewareApp(mwBlock)), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return res.Diagnostics
}

// TestAppMiddleware_UnknownNameIsError : a typo'd built-in name is an error
// with a closest-match suggestion (the runtime can only skip it).
func TestAppMiddleware_UnknownNameIsError(t *testing.T) {
	bag := compileMiddleware(t, `  middleware:
    - mask_secret
`)
	if !hasCode(bag.Errors(), diagnostic.CodeUnknownMiddleware) {
		t.Fatalf("unknown app middleware must be DGT error, got:\n%s", formatDiags(bag))
	}
}

// TestAppMiddleware_BuiltinsClean : the documented built-ins compile cleanly.
func TestAppMiddleware_BuiltinsClean(t *testing.T) {
	bag := compileMiddleware(t, `  middleware:
    - mask_secrets
    - content_filter:
        block_patterns: ["forbidden"]
    - name: response_filter
      config:
        max_length: 100
`)
	if hasCode(bag.Errors(), diagnostic.CodeUnknownMiddleware) {
		t.Fatalf("documented built-ins must not error, got:\n%s", formatDiags(bag))
	}
}

// TestAppMiddleware_CustomRequiresModuleAndKind : a `custom` entry without
// module/kind cannot be dispatched to a worker, so it is a compile-time error
// (rather than a silent runtime skip).
func TestAppMiddleware_CustomRequiresModuleAndKind(t *testing.T) {
	bag := compileMiddleware(t, `  middleware:
    - name: custom
      config:
        timeout: 5
`)
	errs := bag.Errors()
	n := 0
	for _, d := range errs {
		if d.Code == diagnostic.CodeUnknownMiddleware {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("custom without module+kind must yield 2 errors (module + kind), got %d:\n%s",
			n, formatDiags(bag))
	}
}

// TestAppMiddleware_CustomWellFormedClean : a `custom` entry naming a worker
// module + kind compiles without error.
func TestAppMiddleware_CustomWellFormedClean(t *testing.T) {
	bag := compileMiddleware(t, `  middleware:
    - name: custom
      config:
        module: upper_mw
        kind: mw-pool
        timeout: 5
        fail_open: true
`)
	if hasCode(bag.Errors(), diagnostic.CodeUnknownMiddleware) {
		t.Fatalf("well-formed custom middleware must not error, got:\n%s", formatDiags(bag))
	}
}
