//go:build treesitter

package filesystem

import (
	"encoding/json"
	"testing"
	"time"
)

func warmGraph(root string) bool {
	for i := 0; i < 6000; i++ {
		if sc := codeContextFor(root, 1<<20, "charge.go", 1); len(sc.Imports) > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestEnrichment_GraphContextReachesAgent(t *testing.T) {
	dir := t.TempDir()
	writeBillingRepo(t, dir)
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	emb := fakeEmb{dim: 256}
	if !warmSindex(dir, emb) {
		t.Fatal("sindex never warmed")
	}
	if !warmGraph(dir) {
		t.Fatal("code graph never warmed")
	}

	g := buildGraph(dir, 1<<20)
	if !containsSub(g.callers["recordPayment"], "ChargeCard") {
		t.Errorf("graph wrong: recordPayment callers = %v, want a ChargeCard caller", g.callers["recordPayment"])
	}
	if !containsSub(g.callers["ChargeCard"], "HandleCheckout") {
		t.Errorf("graph wrong: ChargeCard callers = %v, want a HandleCheckout caller", g.callers["ChargeCard"])
	}
	if !containsSub(g.imports["charge.go"], "fmt") || !containsSub(g.imports["charge.go"], "errors") {
		t.Errorf("graph wrong: charge.go imports = %v, want fmt+errors", g.imports["charge.go"])
	}

	off, err := m.grep(grepCtx(dir, nil, false), mustJSON(map[string]any{"pattern": "recordPayment", "semantic": "off"}))
	if err != nil || !off.Success {
		t.Fatalf("off: %v %v", err, off.Error)
	}
	on, err := m.grep(grepCtx(dir, emb, true), mustJSON(map[string]any{"pattern": "recordPayment"}))
	if err != nil || !on.Success {
		t.Fatalf("on: %v %v", err, on.Error)
	}

	offJSON, _ := json.MarshalIndent(off.Data, "", "  ")
	onJSON, _ := json.MarshalIndent(on.Data, "", "  ")
	t.Logf("\n--- agent context WITHOUT code-intel (grep recordPayment, semantic:off) ---\n%s", offJSON)
	t.Logf("\n--- agent context WITH code-intel (grep recordPayment enriched) ---\n%s", onJSON)

	hits := relatedHits(t, on.Data.(map[string]any))
	if len(hits) == 0 {
		t.Fatal("enriched grep returned no related hits")
	}
	var decorated *sHit
	for i := range hits {
		if hits[i].Symbol != "" || len(hits[i].Callers) > 0 || len(hits[i].Imports) > 0 {
			decorated = &hits[i]
			break
		}
	}
	if decorated == nil {
		t.Fatal("related hits carry NO graph context (symbol/callers/imports) — the graph is not reaching the agent")
	}
	t.Logf("decorated related hit delivered to agent: symbol=%q callers=%v imports=%v",
		decorated.Symbol, decorated.Callers, decorated.Imports)
	if decorated.Symbol == "" && len(decorated.Callers) == 0 {
		t.Error("decorated hit must carry at least an enclosing symbol or callers")
	}
}
