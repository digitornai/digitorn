package filesystem

import (
	"context"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeEmb struct{ dim int }

func (f fakeEmb) EmbedModel(_ context.Context, _, _ string, texts []string) ([][]float32, int, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[int(h.Sum32())%f.dim]++
		}
		var ss float64
		for _, x := range v {
			ss += float64(x) * float64(x)
		}
		if ss > 0 {
			n := float32(math.Sqrt(ss))
			for j := range v {
				v[j] /= n
			}
		}
		out[i] = v
	}
	return out, f.dim, nil
}

func TestSindex_BuildAndSearch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("deploy.go", "package x\nfunc Deploy() { /* deploy the application to the production server */ }")
	write("cake.go", "package x\nfunc Bake() { /* a chocolate cake recipe with butter */ }")
	// ignored dir must be skipped
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("node_modules/junk.go", "func Deploy() { deploy server application }")

	si := &sindex{root: dir, maxBytes: 1 << 20}
	emb := fakeEmb{dim: 64}
	si.maybeBuild(emb, "x")

	ready := false
	for i := 0; i < 300; i++ {
		si.mu.Lock()
		ready = si.ready && !si.building
		si.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("sindex never became ready")
	}

	hits := si.search(context.Background(), emb, "x", "deploy the application to the server", 3)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Path != "deploy.go" {
		t.Errorf("top hit = %q, want deploy.go", hits[0].Path)
	}
	for _, h := range hits {
		if strings.HasPrefix(h.Path, "node_modules") {
			t.Errorf("node_modules should be skipped, got %q", h.Path)
		}
	}
}
