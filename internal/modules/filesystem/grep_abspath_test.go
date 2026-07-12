package filesystem

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

// grep with an ABSOLUTE path inside the workdir must scan it (not silently
// return scanned:0). Uses the real path policy to know the workdir root.
func TestGrep_AbsolutePathInsideWorkdir(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "src/a.ts", "content": "const useState = 1\n"}))
	pp, _ := workdir.PathPolicyFromContext(ctx)
	absSrc := filepath.Join(pp.Root(), "src")
	r, err := m.grep(ctx, mustJSON(map[string]any{
		"pattern": "useState", "path": absSrc, "output_mode": "count",
	}))
	if err != nil || !r.Success {
		t.Fatalf("abs-path grep: %v %v", err, r.Error)
	}
	got := fmt.Sprint(r.Data)
	if strings.Contains(got, "scanned:0") || strings.Contains(got, `"count":0`) {
		t.Fatalf("absolute path silently scanned nothing: %v", r.Data)
	}
	t.Logf("abs-path grep data: %v", r.Data)
}
