package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
)

// moduleMwApp splices a per-module middleware list under
// tools.modules.filesystem.middleware. mwList items are indented 8 spaces.
func moduleMwApp(mwList string) string {
	return `schema_version: 2
app:
  app_id: mwmod
  name: MwMod
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
      middleware:
` + mwList + `
  capabilities:
    default_policy: auto
`
}

func compileModuleMw(t *testing.T, mwList string) *diagnostic.Bag {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(moduleMwApp(mwList)), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return res.Diagnostics
}

// TestModuleMiddleware_ToolCallClean : the tool-call middleware are valid on the
// per-module surface.
func TestModuleMiddleware_ToolCallClean(t *testing.T) {
	bag := compileModuleMw(t, "        - retry: {}\n        - dedup: {}\n        - circuit_breaker: {}\n")
	if bag.HasErrors() {
		t.Fatalf("tool-call middleware must be valid per-module, got:\n%s", formatDiags(bag))
	}
}

// TestModuleMiddleware_AppLevelRejected : an app-level middleware on the
// per-module surface is the wrong layer — error.
func TestModuleMiddleware_AppLevelRejected(t *testing.T) {
	bag := compileModuleMw(t, "        - mask_secrets: {}\n")
	if !hasCode(bag.Errors(), diagnostic.CodeUnknownMiddleware) {
		t.Fatalf("an app-level middleware per-module must error, got:\n%s", formatDiags(bag))
	}
}

// TestAppMiddleware_ToolCallRejected : a tool-call middleware on
// runtime.middleware is the mirror mistake — error.
func TestAppMiddleware_ToolCallRejected(t *testing.T) {
	bag := compileMiddleware(t, `  middleware:
    - retry
`)
	if !hasCode(bag.Errors(), diagnostic.CodeUnknownMiddleware) {
		t.Fatalf("a tool-call middleware on runtime.middleware must error, got:\n%s", formatDiags(bag))
	}
}

// TestModuleMiddleware_ConfigBoundsError : retry.max_attempts must be >= 1.
func TestModuleMiddleware_ConfigBoundsError(t *testing.T) {
	bag := compileModuleMw(t, "        - retry:\n            max_attempts: 0\n")
	if !hasCode(bag.Errors(), diagnostic.CodeOutOfRange) {
		t.Fatalf("retry.max_attempts:0 must be out of range, got:\n%s", formatDiags(bag))
	}
}

// TestModuleMiddleware_ConfigValidClean : a sane config compiles clean.
func TestModuleMiddleware_ConfigValidClean(t *testing.T) {
	bag := compileModuleMw(t, "        - retry:\n            max_attempts: 3\n        - semantic_cache:\n            similarity_threshold: 0.9\n")
	if hasCode(bag.Errors(), diagnostic.CodeOutOfRange) {
		t.Fatalf("valid middleware config must not error, got:\n%s", formatDiags(bag))
	}
}

// TestModuleMiddleware_SimilarityOutOfRange : semantic_cache.similarity_threshold
// must be in [0,1].
func TestModuleMiddleware_SimilarityOutOfRange(t *testing.T) {
	bag := compileModuleMw(t, "        - semantic_cache:\n            similarity_threshold: 1.5\n")
	if !hasCode(bag.Errors(), diagnostic.CodeOutOfRange) {
		t.Fatalf("similarity_threshold:1.5 must be out of range, got:\n%s", formatDiags(bag))
	}
}

// TestAppMiddleware_ConfigBoundsError : response_filter.max_length must be >= 0.
func TestAppMiddleware_ConfigBoundsError(t *testing.T) {
	bag := compileMiddleware(t, `  middleware:
    - name: response_filter
      config:
        max_length: -1
`)
	if !hasCode(bag.Errors(), diagnostic.CodeOutOfRange) {
		t.Fatalf("response_filter.max_length:-1 must be out of range, got:\n%s", formatDiags(bag))
	}
}
