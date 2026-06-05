package tui

import (
	"strings"
	"testing"
)

// payload builds a tool_result payload with a single text part carrying raw.
func toolPayload(name, raw string) map[string]any {
	return map[string]any{
		"name":  name,
		"parts": []any{map[string]any{"type": "text", "text": raw}},
	}
}

func TestFormatShellOutput_NoJSON(t *testing.T) {
	raw := `{"stdout":"hello\nworld\n","stderr":"","exit_code":0,"cwd":"/x","shell":"bash"}`
	got := formatToolResult(toolPayload("bash.run", raw))
	if got != "hello\nworld" {
		t.Fatalf("shell output = %q, want %q", got, "hello\nworld")
	}
	if strings.Contains(got, "{") || strings.Contains(got, "exit_code") {
		t.Fatalf("shell output still leaks JSON: %q", got)
	}
}

func TestFormatShellOutput_SilentAndError(t *testing.T) {
	if got := formatToolResult(toolPayload("bash.run", `{"stdout":"","stderr":"","exit_code":0}`)); got != "(no output)" {
		t.Fatalf("silent command = %q, want (no output)", got)
	}
	got := formatToolResult(toolPayload("bash.run", `{"stdout":"","stderr":"boom","exit_code":2}`))
	if !strings.Contains(got, "boom") || !strings.Contains(got, "exit 2") {
		t.Fatalf("error command = %q, want stderr + exit 2", got)
	}
}

func TestFormatGlobOutput(t *testing.T) {
	raw := `{"files":["a/b.go","c.go"],"count":2,"truncated":false}`
	got := formatToolResult(toolPayload("filesystem.glob", raw))
	if got != "a/b.go\nc.go" {
		t.Fatalf("glob = %q, want list of files", got)
	}
}

func TestFormatGrepOutput_Matches(t *testing.T) {
	raw := `{"matches":[{"file":"main.go","line":12,"text":"  TODO fix"},{"file":"x.go","line":3,"text":"TODO"}],"truncated":false}`
	got := formatToolResult(toolPayload("filesystem.grep", raw))
	if !strings.Contains(got, "main.go:12  TODO fix") || !strings.Contains(got, "x.go:3  TODO") {
		t.Fatalf("grep matches = %q", got)
	}
	if strings.Contains(got, "{") {
		t.Fatalf("grep still leaks JSON: %q", got)
	}
}

func TestFormatGrepOutput_Count(t *testing.T) {
	if got := formatToolResult(toolPayload("filesystem.grep", `{"count":7}`)); got != "7 matches" {
		t.Fatalf("grep count = %q, want '7 matches'", got)
	}
}

func TestFormatMutationOutput(t *testing.T) {
	if got := formatToolResult(toolPayload("filesystem.write", `{"action":"created","bytes":203,"path":"x"}`)); got != "created · 203 bytes" {
		t.Fatalf("write summary = %q", got)
	}
	if got := formatToolResult(toolPayload("filesystem.edit", `{"replacements":3,"strategy":"exact"}`)); got != "3 replacements" {
		t.Fatalf("edit summary = %q", got)
	}
}

func TestFormatToolResult_ReadPassesThrough(t *testing.T) {
	// read returns a plain (non-JSON) string — must be left untouched.
	body := "1\tpackage main\n2\tfunc main() {}"
	if got := formatToolResult(toolPayload("filesystem.read", body)); got != body {
		t.Fatalf("read output mangled: %q", got)
	}
}

func TestFormatToolResult_Error(t *testing.T) {
	p := map[string]any{"name": "bash.run", "error": "permission denied"}
	if got := formatToolResult(p); got != "permission denied" {
		t.Fatalf("error path = %q, want the error text", got)
	}
}

// run_parallel often double-encodes its task list as a JSON STRING — the
// extractors must decode it, both for the header names and the per-task args.
func TestParallelNames_StringEncoded(t *testing.T) {
	args := map[string]any{"tasks": `[{"tool":"filesystem.read"},{"tool":"filesystem.read"}]`}
	if got := parallelNames(args); got != "read *2" {
		t.Fatalf("string-encoded parallelNames = %q, want %q", got, "read *2")
	}
}

func TestParallelTaskArgs(t *testing.T) {
	args := map[string]any{"tasks": `[
		{"tool":"filesystem.read","args":{"path":"task1.txt"}},
		{"tool":"filesystem.write","args":{"path":"src/App.jsx","content":"x"}},
		{"tool":"bash.run","args":{"command":"ls -la"}}
	]`}
	got := parallelTaskArgs(args)
	want := []string{"task1.txt", "App.jsx", "ls -la"}
	if len(got) != len(want) {
		t.Fatalf("parallelTaskArgs len = %d, want %d : %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parallelTaskArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFormatBackgroundOutput(t *testing.T) {
	raw := `{"name":"bash__run","started_at_unix":1780330423,"state":"running","task_id":"b94673e1-150a-4788-ac79-986758e55aca"}`
	got := formatToolResult(toolPayload("context_builder.background_run", raw))
	if got != "bash.run · running · b94673e1" {
		t.Fatalf("background output = %q, want %q", got, "bash.run · running · b94673e1")
	}
	if strings.Contains(got, "{") || strings.Contains(got, "started_at_unix") {
		t.Fatalf("background output still leaks JSON: %q", got)
	}
}

func TestToolVerb(t *testing.T) {
	cases := map[string]string{
		"run": "$", "bash": "$", "write": "Wrote", "edit": "Edit",
		"read": "Read", "glob": "Search", "grep": "Grep",
		"background_run": "Background", "frobnicate": "frobnicate",
	}
	for action, want := range cases {
		if got := toolVerb(action); got != want {
			t.Errorf("toolVerb(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestCleanToolName(t *testing.T) {
	if got := cleanToolName("bash__run"); got != "bash.run" {
		t.Fatalf("cleanToolName = %q, want bash.run", got)
	}
}

func TestFormatParallelOutput(t *testing.T) {
	raw := `{"results":[{"name":"filesystem__write","status":"completed"},{"name":"bash__run","status":"errored","error":"boom happened here"},{"name":"filesystem__read","status":"completed"}]}`
	got := formatToolResult(toolPayload("context_builder.run_parallel", raw))
	for _, want := range []string{"✓ write", "✗ run", "boom", "✓ read"} {
		if !strings.Contains(got, want) {
			t.Fatalf("parallel group missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "filesystem.") || strings.Contains(got, "bash.") {
		t.Fatalf("parallel group must strip the module prefix:\n%s", got)
	}
	if strings.Contains(got, "{") || strings.Contains(got, "status") {
		t.Fatalf("parallel output still leaks JSON:\n%s", got)
	}
}

func TestParallelNames(t *testing.T) {
	args := map[string]any{"tasks": []any{
		map[string]any{"tool": "filesystem__read", "args": map[string]any{}},
		map[string]any{"tool": "filesystem.write"},
		map[string]any{"name": "bash.run"},
	}}
	if got := parallelNames(args); got != "read · write · run" {
		t.Fatalf("parallelNames = %q, want %q", got, "read · write · run")
	}
	// Duplicates collapse with a *N multiplier.
	dup := map[string]any{"tasks": []any{
		map[string]any{"tool": "bash.run"},
		map[string]any{"tool": "filesystem.glob"},
		map[string]any{"tool": "filesystem.glob"},
	}}
	if got := parallelNames(dup); got != "run · glob *2" {
		t.Fatalf("parallelNames duplicates = %q, want %q", got, "run · glob *2")
	}
	// Overflow : distinct names capped with an ellipsis.
	big := map[string]any{"calls": []any{
		map[string]any{"tool": "a"}, map[string]any{"tool": "b"}, map[string]any{"tool": "c"},
		map[string]any{"tool": "d"}, map[string]any{"tool": "e"}, map[string]any{"tool": "f"},
	}}
	if got := parallelNames(big); !strings.HasPrefix(got, "a · b · c · d") || !strings.HasSuffix(got, "· …") {
		t.Fatalf("parallelNames overflow = %q, want 'a · b · c · d · …'", got)
	}
}
