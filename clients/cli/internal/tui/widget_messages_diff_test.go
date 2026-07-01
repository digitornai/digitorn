package tui

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

const diffPayload = `--- a/poc.txt
+++ b/poc.txt
@@ -1,2 +1,2 @@
 keep this
-old answer
+new answer`

// A completed edit/write chip carries a unified diff : collapsed it shows the
// +A -D stat, expanded it shows the coloured diff body instead of the plain
// output preview.
func TestRenderTool_DiffChip(t *testing.T) {
	m := NewMessages(theme.Default())
	m.SetSize(80, 24)
	msg := client.Message{
		Role:     "tool",
		Content:  "filesystem.edit",
		CallID:   "call_1",
		Status:   "completed",
		Seq:      2,
		ToolDiff: diffPayload,
		ToolArg:  "poc.txt",
	}

	// Collapsed : the stat is the hint, the diff body is hidden.
	collapsed := stripANSI(m.renderTool(msg, 80))
	if !strings.Contains(collapsed, "+1 -1") {
		t.Fatalf("collapsed diff chip should show the +1 -1 stat:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "new answer") {
		t.Fatalf("collapsed chip must not reveal the diff body:\n%s", collapsed)
	}

	// Expanded : the coloured diff body is shown.
	m.expanded["call_1"] = true
	expanded := stripANSI(m.renderTool(msg, 80))
	for _, want := range []string{"new answer", "old answer", "keep this"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded diff chip missing %q:\n%s", want, expanded)
		}
	}
}

// toolDiffText pulls the unified diff off a tool_result payload, and is empty
// for a payload without one (non-mutating tools).
func TestToolDiffText(t *testing.T) {
	if got := toolDiffText(map[string]any{"unified_diff": diffPayload}); got != diffPayload {
		t.Fatalf("toolDiffText did not extract unified_diff:\n%s", got)
	}
	if got := toolDiffText(map[string]any{"output": "ok"}); got != "" {
		t.Fatalf("toolDiffText on a non-diff payload = %q, want empty", got)
	}
}
