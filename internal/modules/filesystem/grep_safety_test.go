package filesystem

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

// TestGrep_BoundsAndSanitizesMatchLines pins the fix for "a complex grep crashes
// the terminal that runs the CLI". A match in a minified/generated file (one
// multi-megabyte line) must NOT come back whole, and raw control bytes (ESC, CR,
// BEL, backspace) — which corrupt or crash a terminal — must be neutralised. A
// grep result has to be safe to render anywhere.
func TestGrep_BoundsAndSanitizesMatchLines(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: home})
	ctx := workdir.WithPathPolicy(context.Background(), pp)

	// Line 1 : ~1 MB single "minified" line that matches token.*accum.
	// Line 2 : a match wrapped in terminal-corrupting control bytes.
	giant := "head " + strings.Repeat("token_accumulator ", 60000) + " tail"
	ctrl := "x token_count \x1b[2J\r\x07\x08 y"
	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "min.js", "content": giant + "\n" + ctrl + "\n"})); err != nil || !r.Success {
		t.Fatalf("write: %v (%v)", err, r.Error)
	}

	r, err := m.grep(ctx, mustJSON(map[string]any{"pattern": "token.*accum|token.*count"}))
	if err != nil || !r.Success {
		t.Fatalf("grep: %v (%v)", err, r.Error)
	}
	data, _ := r.Data.(map[string]any)
	matches, _ := data["matches"].([]grepMatch)
	if len(matches) < 2 {
		t.Fatalf("expected both lines to match, got %d", len(matches))
	}

	const ceiling = maxMatchLineBytes + len(" …[line truncated]")
	sawTruncated := false
	for _, mm := range matches {
		if len(mm.Text) > ceiling {
			t.Fatalf("match text unbounded: %d bytes (ceiling %d)", len(mm.Text), ceiling)
		}
		if strings.ContainsAny(mm.Text, "\x1b\r\x07\x08\x00") {
			t.Fatalf("match text still carries control bytes: %q", mm.Text)
		}
		if strings.Contains(mm.Text, "[line truncated]") {
			sawTruncated = true
		}
	}
	if !sawTruncated {
		t.Fatal("the giant line should have been truncated with a marker")
	}
}
