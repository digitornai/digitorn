//go:build !treesitter

package filesystem

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/context/repomap"
)

func TestFallbackRepoMap(t *testing.T) {
	_, ws := setupFS(t)
	writeFile(t, ws, "pay.go", "package pay\n\nfunc ChargeCard(amt int) error { return nil }\ntype Invoice struct{}\n")
	writeFile(t, ws, "sub/util.go", "package sub\n\nfunc Helper() {}\n")
	writeFile(t, ws, "node_modules/junk.js", "function ignored() {}\n") // skip-dir, must be excluded

	g := fallbackRepoGraph(ws)
	if len(g.Syms) == 0 {
		t.Fatal("fallback repo-map extracted no symbols — the no-treesitter codebase map would be empty")
	}
	out := repomap.Render(g, 6000)
	for _, want := range []string{"pay.go", "func ChargeCard", "type Invoice", "sub/util.go", "func Helper"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo-map render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ignored") || strings.Contains(out, "node_modules") {
		t.Errorf("repo-map leaked a skip-dir file:\n%s", out)
	}
}

func TestDefName(t *testing.T) {
	cases := map[string]string{
		"func ChargeCard(amt int) error": "ChargeCard",
		"type Invoice struct{}":          "Invoice",
		"func (m *Module) read(":         "read",
		"export class PaymentService {":  "PaymentService",
		"def process_payment(self):":     "process_payment",
	}
	for sig, want := range cases {
		if got := defName(sig); got != want {
			t.Errorf("defName(%q) = %q, want %q", sig, got, want)
		}
	}
}
