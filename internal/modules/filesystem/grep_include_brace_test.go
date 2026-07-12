package filesystem

import (
	"fmt"
	"strings"
	"testing"
)

// grep include must support brace expansion (*.{ts,tsx}) like the glob tool —
// otherwise a common include silently matches nothing (scanned: 0).
func TestGrep_IncludeBraceExpansion(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "src/a.ts", "content": "const useState = 1\n"}))
	m.write(ctx, mustJSON(map[string]any{"path": "src/b.tsx", "content": "const useEffect = 2\n"}))
	m.write(ctx, mustJSON(map[string]any{"path": "src/c.js", "content": "const useState = 3\n"}))

	r, err := m.grep(ctx, mustJSON(map[string]any{
		"pattern": "useState|useEffect", "include": "*.{ts,tsx}", "output_mode": "count",
	}))
	if err != nil || !r.Success {
		t.Fatalf("grep brace include: %v %v", err, r.Error)
	}
	got := fmt.Sprint(r.Data)
	// must match a.ts + b.tsx (2), NOT c.js
	if !strings.Contains(got, "2") {
		t.Fatalf("brace include *.{ts,tsx} matched wrong count (expected 2): %v", r.Data)
	}
}

// a plain (non-brace) include still works.
func TestGrep_IncludePlainStillWorks(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "a.ts", "content": "hit here\n"}))
	m.write(ctx, mustJSON(map[string]any{"path": "a.md", "content": "hit here\n"}))
	r, _ := m.grep(ctx, mustJSON(map[string]any{"pattern": "hit", "include": "*.ts", "output_mode": "files_with_matches"}))
	got := fmt.Sprint(r.Data)
	if !strings.Contains(got, "a.ts") || strings.Contains(got, "a.md") {
		t.Fatalf("plain include *.ts broken: %v", r.Data)
	}
}
