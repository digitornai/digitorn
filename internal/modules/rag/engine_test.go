package rag

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"testing"
)

// fakeEmbedder is a deterministic bag-of-words embedder : token overlap
// drives cosine similarity, enough to exercise ranking without ONNX.
type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) EmbedModel(_ context.Context, _, _ string, texts []string) ([][]float32, int, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[int(h.Sum32())%f.dim] += 1
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

// fakeBackend is an in-memory VectorBackend with cosine search.
type fakeBackend struct {
	dims map[string]int
	docs map[string][]Document
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{dims: map[string]int{}, docs: map[string][]Document{}}
}

func (b *fakeBackend) EnsureKB(_ context.Context, kb string, dim int) error {
	if d, ok := b.dims[kb]; ok && d != dim {
		return errDimMismatch
	}
	b.dims[kb] = dim
	return nil
}
func (b *fakeBackend) DeleteKB(_ context.Context, kb string) error {
	delete(b.dims, kb)
	delete(b.docs, kb)
	return nil
}
func (b *fakeBackend) ListKBs(context.Context) ([]string, error) {
	ks := make([]string, 0, len(b.dims))
	for k := range b.dims {
		ks = append(ks, k)
	}
	return ks, nil
}
func (b *fakeBackend) CountKB(_ context.Context, kb string) (int, error) { return len(b.docs[kb]), nil }
func (b *fakeBackend) Upsert(_ context.Context, kb string, docs []Document) error {
	byID := map[string]int{}
	for i, d := range b.docs[kb] {
		byID[d.ID] = i
	}
	for _, d := range docs {
		if i, ok := byID[d.ID]; ok {
			b.docs[kb][i] = d
		} else {
			b.docs[kb] = append(b.docs[kb], d)
		}
	}
	return nil
}
func (b *fakeBackend) Search(_ context.Context, kb string, vec []float32, topK int, filter Filter) ([]SearchHit, error) {
	var hits []SearchHit
	for _, d := range b.docs[kb] {
		if !filter.Empty() && !metaMatches(d.Meta, filter) {
			continue
		}
		hits = append(hits, SearchHit{Document: d, Score: cosf(vec, d.Vector)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}
func (b *fakeBackend) DeleteBySource(_ context.Context, kb, source string) error {
	kept := b.docs[kb][:0]
	for _, d := range b.docs[kb] {
		if d.Source != source {
			kept = append(kept, d)
		}
	}
	b.docs[kb] = kept
	return nil
}
func (b *fakeBackend) Scan(_ context.Context, kb string) ([]Document, error) {
	out := make([]Document, 0, len(b.docs[kb]))
	for _, d := range b.docs[kb] {
		out = append(out, Document{ID: d.ID, Text: d.Text, Source: d.Source, Chunk: d.Chunk, Meta: d.Meta})
	}
	return out, nil
}
func (b *fakeBackend) Close() error { return nil }

var errDimMismatch = &dimErr{}

type dimErr struct{}

func (*dimErr) Error() string { return "dim mismatch" }

func cosf(a, b []float32) float32 {
	var dot float64
	for i := range a {
		if i < len(b) {
			dot += float64(a[i]) * float64(b[i])
		}
	}
	return float32(dot)
}

func TestEngine_IngestQueryRanks(t *testing.T) {
	cfg, _ := ParseConfig(nil)
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)

	corpus := "Le guide de déploiement explique comment déployer une application sur le serveur de production. " +
		"La recette du gâteau au chocolat demande du beurre et du sucre. " +
		"Les sauvegardes de base de données se planifient chaque nuit."
	n, err := eng.Ingest(context.Background(), "kb1", corpus, "doc.md")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n == 0 {
		t.Fatal("ingested 0 chunks")
	}

	hits, err := eng.Query(context.Background(), "kb1", "comment déployer une application sur le serveur", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if !strings.Contains(strings.ToLower(hits[0].Text), "déplo") {
		t.Errorf("top hit not the deployment chunk: %q (score %.3f)", hits[0].Text, hits[0].Score)
	}
	if hits[0].Source != "doc.md" {
		t.Errorf("citation source = %q", hits[0].Source)
	}
}

func TestEngine_ReingestUpdatesInPlace(t *testing.T) {
	cfg, _ := ParseConfig(nil)
	be := newFakeBackend()
	eng := NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil)
	ctx := context.Background()

	_, _ = eng.Ingest(ctx, "kb1", "alpha beta gamma delta", "s1")
	first, _ := be.CountKB(ctx, "kb1")
	_, _ = eng.Ingest(ctx, "kb1", "alpha beta gamma delta", "s1")
	second, _ := be.CountKB(ctx, "kb1")
	if first != second {
		t.Errorf("re-ingest duplicated: %d → %d", first, second)
	}
}

func TestEngine_CitationsFormat(t *testing.T) {
	hits := []SearchHit{
		{Document: Document{Source: "a.md", Chunk: 0}, Score: 0.9},
		{Document: Document{Source: "b.md", Chunk: 2}, Score: 0.7},
	}
	inline := formatCitations(hits, "inline")
	if !strings.Contains(inline, "a.md#0") || !strings.Contains(inline, "[2]") {
		t.Errorf("inline citations wrong: %q", inline)
	}
	foot := formatCitations(hits, "footnote")
	if !strings.Contains(foot, "[^1]:") {
		t.Errorf("footnote citations wrong: %q", foot)
	}
}
