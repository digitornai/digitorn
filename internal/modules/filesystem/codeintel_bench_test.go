package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// --- test embedders (fakeEmb lives in sindex_test.go) ---

type slowEmb struct {
	d   time.Duration
	dim int
}

func (s slowEmb) EmbedModel(ctx context.Context, model, role string, texts []string) ([][]float32, int, error) {
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	return fakeEmb{dim: s.dim}.EmbedModel(ctx, model, role, texts)
}

type panicEmb struct{}

func (panicEmb) EmbedModel(context.Context, string, string, []string) ([][]float32, int, error) {
	panic("boom")
}

// --- helpers ---

func grepCtx(root string, emb module.Embedder, autoIndex bool) context.Context {
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: root})
	ctx := workdir.WithPathPolicy(context.Background(), pp)
	if emb != nil {
		ctx = module.WithEmbedder(ctx, emb)
	}
	if autoIndex {
		ctx = module.WithModuleConfig(ctx, map[string]any{"auto_index_workdir": true})
	}
	return ctx
}

func warmSindex(root string, emb module.Embedder) bool {
	si := sindexes.get(root, 1<<20)
	si.maybeBuild(emb, "x")
	for i := 0; i < 6000; i++ { // up to ~60s for large corpora
		si.mu.Lock()
		ready := si.ready && !si.building
		si.mu.Unlock()
		if ready {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func writeRepo(t testing.TB, dir string) {
	files := map[string]string{
		"deploy.go": "package x\nfunc Deploy() { /* deploy the application to the production server */ }\n",
		"cake.go":   "package x\nfunc Bake() { /* chocolate cake recipe with butter */ }\n",
		"db.go":     "package x\nfunc Backup() { /* nightly database backups */ }\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func nMatches(data map[string]any) int {
	v := data["matches"]
	if v == nil {
		return 0
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return 0
	}
	return rv.Len()
}

// --- regression : enrichment must NOT change the exact matches, and grep
// must keep working when the index is absent/slow/broken ---

func TestGrep_EnrichmentIsAdditive(t *testing.T) {
	dir := t.TempDir()
	writeRepo(t, dir)

	// Exact-only (semantic off / no config).
	rOff, err := (&Module{cfg: Config{MaxFileBytes: 1 << 20}}).grep(
		grepCtx(dir, nil, false), mustJSON(map[string]any{"pattern": "Deploy", "semantic": "off"}))
	if err != nil || !rOff.Success {
		t.Fatalf("off: %v %v", err, rOff.Error)
	}
	off := nMatches(rOff.Data.(map[string]any))

	// Enriched (warm index + fast embedder + auto_index_workdir).
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	if !warmSindex(dir, fakeEmb{dim: 64}) {
		t.Fatal("index never warmed")
	}
	rOn, err := m.grep(grepCtx(dir, fakeEmb{dim: 64}, true), mustJSON(map[string]any{"pattern": "Deploy"}))
	if err != nil || !rOn.Success {
		t.Fatalf("on: %v %v", err, rOn.Error)
	}
	on := nMatches(rOn.Data.(map[string]any))

	if off != on {
		t.Errorf("enrichment changed exact matches: off=%d on=%d", off, on)
	}
	if _, hasRelated := rOff.Data.(map[string]any)["related"]; hasRelated {
		t.Error("semantic:off must not produce related results")
	}
}

func TestGrep_SlowEmbedderIsBounded(t *testing.T) {
	dir := t.TempDir()
	writeRepo(t, dir)
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	if !warmSindex(dir, fakeEmb{dim: 64}) {
		t.Fatal("index never warmed")
	}
	// Query embed sleeps 3s ; grep must still return (exact-only) within the
	// enrich budget, NOT block for 3s.
	ctx := grepCtx(dir, slowEmb{d: 3 * time.Second, dim: 64}, true)
	start := time.Now()
	r, err := m.grep(ctx, mustJSON(map[string]any{"pattern": "Deploy"}))
	elapsed := time.Since(start)
	if err != nil || !r.Success {
		t.Fatalf("grep: %v %v", err, r.Error)
	}
	if elapsed > time.Second {
		t.Errorf("slow embedder blocked grep for %v (budget %v) — must be bounded", elapsed, codeEnrichBudget)
	}
	if nMatches(r.Data.(map[string]any)) == 0 {
		t.Error("exact matches must still be returned under a slow embedder")
	}
}

func TestGrep_PanicEmbedderIsSafe(t *testing.T) {
	dir := t.TempDir()
	writeRepo(t, dir)
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	_ = warmSindex(dir, fakeEmb{dim: 64})
	// A panicking embedder must never crash or fail grep.
	r, err := m.grep(grepCtx(dir, panicEmb{}, true), mustJSON(map[string]any{"pattern": "Deploy"}))
	if err != nil || !r.Success {
		t.Fatalf("panic embedder broke grep: %v %v", err, r.Error)
	}
	if nMatches(r.Data.(map[string]any)) == 0 {
		t.Error("exact matches must survive a panicking embedder")
	}
}

// --- benchmarks ---

// BenchmarkGrep_SemanticOff is the base path (no enrichment) — the reference.
func BenchmarkGrep_SemanticOff(b *testing.B) {
	root := buildCorpus(b, 600, 150)
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	ctx := grepCtx(root, nil, false)
	req := mustJSON(map[string]any{"pattern": "errNeedleHere", "semantic": "off"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.grep(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGrep_SemanticOn_Warm measures grep with enrichment over a warm
// index (fast embedder) — the added cost vs the base path.
func BenchmarkGrep_SemanticOn_Warm(b *testing.B) {
	root := buildCorpus(b, 600, 150)
	if !warmSindex(root, fakeEmb{dim: 64}) {
		b.Fatal("index never warmed")
	}
	m := &Module{cfg: Config{MaxFileBytes: 1 << 20}}
	ctx := grepCtx(root, fakeEmb{dim: 64}, true)
	req := mustJSON(map[string]any{"pattern": "errNeedleHere"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.grep(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSindex_Build measures building the ephemeral semantic index over
// a sizeable tree (fast fake embedder isolates the index cost from ONNX).
func BenchmarkSindex_Build(b *testing.B) {
	root := buildCorpus(b, 1000, 120)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		si := &sindex{root: root, maxBytes: 1 << 20}
		_, _ = si.build(fakeEmb{dim: 64}, "x")
	}
	b.StopTimer()
	si := &sindex{root: root, maxBytes: 1 << 20}
	chunks, _ := si.build(fakeEmb{dim: 64}, "x")
	b.Logf("indexed chunks=%d over ~1000 files", len(chunks))
}
