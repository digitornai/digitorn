//go:build treesitter

package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func symsOf(chs []sChunk) string {
	var b strings.Builder
	for _, c := range chs {
		b.WriteString(c.sym)
		b.WriteByte('|')
	}
	return b.String()
}

func TestAstChunks_Go(t *testing.T) {
	src := []byte("package x\nimport \"fmt\"\nfunc Deploy(s string) error { fmt.Println(s); return nil }\ntype Server struct{ Addr string }\nfunc (s *Server) Start() {}\n")
	got := symsOf(astChunks("main.go", src))
	for _, want := range []string{"func Deploy", "type Server", "method Start"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestAstChunks_Python(t *testing.T) {
	src := []byte("def deploy(x):\n    return x\n\nclass Server:\n    def start(self):\n        pass\n")
	got := symsOf(astChunks("app.py", src))
	for _, want := range []string{"func deploy", "class Server", "func start"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestCodeGraph_CallersImportsEnclosing(t *testing.T) {
	dir := t.TempDir()
	src := "package x\nimport \"fmt\"\nfunc Deploy() { helper(); fmt.Println(\"x\") }\nfunc helper() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "deploy.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g := buildGraph(dir, 1<<20)

	if sc := g.context("deploy.go", 3); sc.Symbol != "func Deploy" {
		t.Errorf("enclosing symbol = %q, want func Deploy", sc.Symbol)
	}
	if imps := g.imports["deploy.go"]; len(imps) == 0 || !strings.Contains(strings.Join(imps, ","), "fmt") {
		t.Errorf("imports = %v, want fmt", g.imports["deploy.go"])
	}
	callers := strings.Join(g.callers["helper"], "|")
	if !strings.Contains(callers, "Deploy") {
		t.Errorf("helper callers = %q, want a Deploy caller", callers)
	}
}

func TestAstChunks_UnknownLangNil(t *testing.T) {
	if astChunks("notes.md", []byte("# title")) != nil {
		t.Error("markdown should not produce AST chunks (falls back to line windows)")
	}
}
