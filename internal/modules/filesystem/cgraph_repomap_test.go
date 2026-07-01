//go:build treesitter

package filesystem

import (
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/context/repomap"
)

func TestRepoMap_InjectedAndRanked(t *testing.T) {
	dir := t.TempDir()
	writeBillingRepo(t, dir)

	var m string
	for i := 0; i < 600; i++ {
		m = repomap.Get(dir)
		if m != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m == "" {
		t.Fatal("repo map never produced")
	}
	t.Logf("\n--- codebase map injected into the agent system prompt ---\n%s", m)

	for _, want := range []string{"charge.go:", "checkout.go:", "func ChargeCard", "func HandleCheckout", "func recordPayment"} {
		if !strings.Contains(m, want) {
			t.Errorf("map missing %q", want)
		}
	}
	if strings.Index(m, "charge.go:") > strings.Index(m, "checkout.go:") {
		t.Errorf("charge.go (the call-graph hub) should rank before checkout.go:\n%s", m)
	}
}
