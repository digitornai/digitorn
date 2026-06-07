package repomap

import (
	"strings"
	"testing"
)

func TestRender_RanksHubHigher(t *testing.T) {
	g := Graph{
		Syms: []Sym{
			{Key: "a", Name: "Helper", Kind: "func", File: "util.go", Sig: "func Helper() {}", Line: 1},
			{Key: "b", Name: "Run", Kind: "func", File: "app.go", Sig: "func Run() {}", Line: 1},
			{Key: "c", Name: "Start", Kind: "func", File: "app.go", Sig: "func Start() {}", Line: 5},
			{Key: "d", Name: "Init", Kind: "func", File: "app.go", Sig: "func Init() {}", Line: 9},
		},
		Calls: map[string][]string{
			"b": {"Helper"},
			"c": {"Helper"},
			"d": {"Helper"},
		},
	}
	out := Render(g, 0)
	if out == "" {
		t.Fatal("empty render")
	}
	helperPos := strings.Index(out, "Helper")
	utilHeader := strings.Index(out, "util.go:")
	appHeader := strings.Index(out, "app.go:")
	if utilHeader == -1 || appHeader == -1 || helperPos == -1 {
		t.Fatalf("missing entries in:\n%s", out)
	}
	if utilHeader > appHeader {
		t.Errorf("hub file util.go (Helper called by 3) should rank before app.go:\n%s", out)
	}
}

func TestRender_BudgetRespected(t *testing.T) {
	var syms []Sym
	calls := map[string][]string{}
	for i := 0; i < 500; i++ {
		k := "k" + itoa(i)
		syms = append(syms, Sym{Key: k, Name: "Fn" + itoa(i), Kind: "func", File: "f" + itoa(i%20) + ".go", Sig: "func Fn" + itoa(i) + "(a int, b string) error { ... }", Line: i})
	}
	g := Graph{Syms: syms, Calls: calls}
	out := Render(g, 800)
	if len(out) > 1000 {
		t.Errorf("render exceeded budget: %d chars", len(out))
	}
	if out == "" {
		t.Fatal("budget too small produced nothing")
	}
}

func TestRender_EmptyGraph(t *testing.T) {
	if Render(Graph{}, 0) != "" {
		t.Error("empty graph must render empty")
	}
}

func TestGet_NoProviderIsEmptyAndSafe(t *testing.T) {
	Register(nil)
	if Get("/some/root") != "" {
		t.Error("no provider must yield empty, never block")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
