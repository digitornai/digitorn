package filesystem

import (
	"strings"
	"testing"
)

// grep with only ast_pattern (no regex pattern) must NOT dead-end on
// "pattern must not be empty" — it runs a structural search.
func TestGrep_ASTPatternOnly_NoEmptyError(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "a.ts", "content": "export function add(a: number, b: number) { return a + b; }\n"}))
	r, err := m.grep(ctx, mustJSON(map[string]any{"ast_pattern": "add number", "include": "*.ts"}))
	if err != nil {
		t.Fatalf("ast_pattern-only grep errored: %v (%v)", err, r.Error)
	}
	if !r.Success {
		t.Fatalf("ast_pattern-only grep not successful: %v", r.Error)
	}
	if strings.Contains(r.Error, "pattern must not be empty") {
		t.Fatalf("still dead-ended on empty pattern")
	}
}

// grep with neither pattern nor ast_pattern still errors clearly.
func TestGrep_NoPatternNoAST_Errors(t *testing.T) {
	m, ctx := hardenModule(t)
	r, _ := m.grep(ctx, mustJSON(map[string]any{"include": "*.ts"}))
	if r.Success || !strings.Contains(r.Error, "pattern") {
		t.Fatalf("expected an honest pattern error, got success=%v err=%q", r.Success, r.Error)
	}
}
