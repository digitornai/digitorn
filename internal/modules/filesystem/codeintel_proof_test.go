package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBillingRepo(t testing.TB, dir string) {
	files := map[string]string{
		"charge.go": "package billing\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n)\n\n" +
			"// ChargeCard validates the requested payment and charges the customer card\n" +
			"func ChargeCard(amount int) error {\n\tif amount <= 0 {\n\t\treturn errors.New(\"invalid amount\")\n\t}\n\trecordPayment(amount)\n\treturn nil\n}\n\n" +
			"// recordPayment writes the payment to the ledger\n" +
			"func recordPayment(amount int) {\n\tfmt.Println(\"payment recorded\", amount)\n}\n",
		"checkout.go": "package billing\n\n" +
			"// HandleCheckout runs the customer checkout and charges the card\n" +
			"func HandleCheckout() error {\n\treturn ChargeCard(100)\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func relatedHits(t testing.TB, data map[string]any) []sHit {
	v, ok := data["related"]
	if !ok {
		return nil
	}
	hits, ok := v.([]sHit)
	if !ok {
		t.Fatalf("related is %T, want []sHit", v)
	}
	return hits
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestEnrichment_ActivableToggle(t *testing.T) {
	dir := t.TempDir()
	writeBillingRepo(t, dir)
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	emb := fakeEmb{dim: 256}
	if !warmSindex(dir, emb) {
		t.Fatal("index never warmed")
	}

	cases := []struct {
		name        string
		ctx         context.Context
		semantic    string
		wantRelated bool
	}{
		{"toggle_off_app_did_not_opt_in", grepCtx(dir, emb, false), "", false},
		{"toggle_on_but_no_embedder", grepCtx(dir, nil, true), "", false},
		{"toggle_on_but_call_forces_off", grepCtx(dir, emb, true), "off", false},
		{"toggle_on_and_embedder_present", grepCtx(dir, emb, true), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := map[string]any{"pattern": "recordPayment"}
			if c.semantic != "" {
				req["semantic"] = c.semantic
			}
			r, err := m.grep(c.ctx, mustJSON(req))
			if err != nil || !r.Success {
				t.Fatalf("grep: %v %v", err, r.Error)
			}
			got := len(relatedHits(t, r.Data.(map[string]any))) > 0
			if got != c.wantRelated {
				t.Errorf("%s: related present = %v, want %v", c.name, got, c.wantRelated)
			}
		})
	}
}

func TestEnrichment_PayloadVisible(t *testing.T) {
	dir := t.TempDir()
	writeBillingRepo(t, dir)
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	emb := fakeEmb{dim: 256}
	if !warmSindex(dir, emb) {
		t.Fatal("index never warmed")
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
	t.Logf("\n--- agent context WITHOUT code-intel (grep, semantic:off) ---\n%s", offJSON)
	t.Logf("\n--- agent context WITH code-intel (grep enriched) ---\n%s", onJSON)

	if _, has := off.Data.(map[string]any)["related"]; has {
		t.Error("off path must not carry related")
	}
	hits := relatedHits(t, on.Data.(map[string]any))
	if len(hits) == 0 {
		t.Fatal("enriched grep returned no related code locations — no added value")
	}

	exact := map[string]bool{}
	if ms, ok := off.Data.(map[string]any)["matches"]; ok {
		b, _ := json.Marshal(ms)
		var rows []map[string]any
		_ = json.Unmarshal(b, &rows)
		for _, r := range rows {
			if p, ok := r["file"].(string); ok {
				exact[p] = true
			}
		}
	}
	extra := 0
	for _, h := range hits {
		if !exact[h.Path] {
			extra++
		}
	}
	t.Logf("exact-match files=%d, related code locations surfaced=%d (of which %d in files grep alone did not match)",
		len(exact), len(hits), extra)
}
