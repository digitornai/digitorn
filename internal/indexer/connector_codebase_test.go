//go:build treesitter

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodebaseConnector_Walk(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("deploy.go", "package x\nimport \"fmt\"\nfunc Deploy(s string) error { fmt.Println(s); return nil }\ntype Server struct{ Addr string }\n")
	write("notes.md", "# not code, must be skipped")
	write("node_modules/junk.go", "package j\nfunc Ignored() {}\n")

	conn := &codebaseConnector{}
	spec := SourceSpec{Name: "repo", Type: "codebase", KB: "kb", Opts: map[string]any{"path": dir}}
	docs := map[string]Document{}
	if err := conn.Walk(context.Background(), spec, func(d Document) error {
		docs[d.ID] = d
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	symbols := map[string]bool{}
	for _, d := range docs {
		if p, _ := d.Meta["path"].(string); strings.HasPrefix(p, "node_modules") {
			t.Errorf("node_modules indexed: %s", d.ID)
		}
		if s, _ := d.Meta["symbol"].(string); s != "" {
			symbols[s] = true
		}
	}
	for _, want := range []string{"func Deploy", "type Server"} {
		if !symbols[want] {
			t.Errorf("missing symbol chunk %q (got %v)", want, symbols)
		}
	}
	if symbols["func Ignored"] {
		t.Error("ignored-dir symbol leaked into the index")
	}
}
