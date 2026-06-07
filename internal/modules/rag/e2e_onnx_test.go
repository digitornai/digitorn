//go:build onnx

package rag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/embeddings"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// onnxEmbedder adapts the real multi-model embeddings Manager to the
// module.Embedder the rag module reads from ctx.
type onnxEmbedder struct{ mgr *embeddings.Manager }

func (e onnxEmbedder) EmbedModel(ctx context.Context, model, role string, texts []string) ([][]float32, int, error) {
	vecs, _, dim, err := e.mgr.Embed(ctx, model, role, texts, true)
	return vecs, dim, err
}

// Full pipeline with REAL minilm ONNX embeddings + REAL Qdrant, exercised
// through the module's actual tool handlers (config + embedder injected
// via ctx exactly as the worker does).
//
//	docker run -p 6333:6333 -p 6334:6334 qdrant/qdrant
//	ONNXRUNTIME_LIB=.../bin/onnxruntime.dll QDRANT_URL=localhost:6334 \
//	  go test -tags onnx ./internal/modules/rag/ -run TestRAG_E2E -v
func TestRAG_E2E_RealEmbeddingsRealQdrant(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("set QDRANT_URL (e.g. localhost:6334) to run the live E2E")
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	m := New()
	ctx := context.Background()
	ctx = module.WithAppID(ctx, "e2e-app")
	ctx = module.WithEmbedder(ctx, onnxEmbedder{mgr: mgr})
	ctx = module.WithModuleConfig(ctx, map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": url},
		"citations":       map[string]any{"enabled": true, "format": "inline"},
	})

	_, _ = m.deleteKB(ctx, raw(`{"name":"e2e"}`)) // clean slate

	corpus := "Le guide de déploiement explique comment déployer une application sur le serveur de production. " +
		"La recette du gâteau au chocolat demande du beurre et du sucre. " +
		"Les sauvegardes de la base de données tournent chaque nuit."
	res, _ := m.ingest(ctx, raw(`{"knowledge_base":"e2e","source":"doc.md","text":`+jsonStr(corpus)+`}`))
	if !res.Success {
		t.Fatalf("ingest: %s", res.Error)
	}
	if n, _ := res.Data.(map[string]any)["chunks"].(int); n == 0 {
		t.Fatalf("ingested 0 chunks")
	}

	res, _ = m.query(ctx, raw(`{"knowledge_base":"e2e","query":"comment déployer une application sur le serveur","top_k":3}`))
	if !res.Success {
		t.Fatalf("query: %s", res.Error)
	}
	data, _ := res.Data.(map[string]any)
	results, _ := data["results"].([]map[string]any)
	if len(results) == 0 {
		t.Fatal("query returned no results")
	}
	top, _ := results[0]["text"].(string)
	t.Logf("top result: %q (score %v)", top, results[0]["score"])
	if !strings.Contains(strings.ToLower(top), "déplo") {
		t.Errorf("top result is not the deployment chunk: %q", top)
	}
	if src, _ := results[0]["source"].(string); src != "doc.md" {
		t.Errorf("citation source = %q, want doc.md", src)
	}
	if c, _ := data["citations"].(string); c == "" {
		t.Error("no citations rendered")
	}

	_, _ = m.deleteKB(ctx, raw(`{"name":"e2e"}`))
}

type onnxReranker struct{ mgr *embeddings.Manager }

func (r onnxReranker) Rerank(ctx context.Context, model, query string, docs []string) ([]float32, error) {
	scores, _, err := r.mgr.Rerank(ctx, model, query, docs)
	return scores, err
}

// Full RAG with reranking : hybrid retrieve → real bge-reranker cross-encoder
// reorders → top result is the deployment doc. Real ONNX + real Qdrant.
func TestRAG_E2E_Rerank(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("set QDRANT_URL to run the live rerank E2E")
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	m := New()
	ctx := context.Background()
	ctx = module.WithAppID(ctx, "e2e-rerank")
	ctx = module.WithEmbedder(ctx, onnxEmbedder{mgr: mgr})
	ctx = module.WithReranker(ctx, onnxReranker{mgr: mgr})
	ctx = module.WithModuleConfig(ctx, map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": url},
		"reranker":        true,
		"pipeline":        map[string]any{"rerank_top_n": 10, "final_top_k": 3},
	})

	_, _ = m.deleteKB(ctx, raw(`{"name":"rr"}`))
	docs := map[string]string{
		"deploy":  "How to deploy an application to the production server step by step.",
		"cake":    "A traditional recipe for molten chocolate cake with butter and sugar.",
		"backup":  "Nightly database backups are scheduled and rotated weekly.",
		"weather": "The weather forecast predicts rain over the weekend in the valley.",
	}
	for src, text := range docs {
		res, _ := m.ingest(ctx, raw(`{"knowledge_base":"rr","source":"`+src+`","text":`+jsonStr(text)+`}`))
		if !res.Success {
			t.Fatalf("ingest %s: %s", src, res.Error)
		}
	}

	res, _ := m.query(ctx, raw(`{"knowledge_base":"rr","query":"how do I deploy an app to the server","top_k":3}`))
	if !res.Success {
		t.Fatalf("query: %s", res.Error)
	}
	results, _ := res.Data.(map[string]any)["results"].([]map[string]any)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	top, _ := results[0]["source"].(string)
	t.Logf("reranked top source=%q score=%v", top, results[0]["score"])
	if top != "deploy" {
		t.Errorf("rerank top source = %q, want deploy", top)
	}
	_, _ = m.deleteKB(ctx, raw(`{"name":"rr"}`))
}

// Ingest a real multi-file directory (md/txt/html) then query — proves
// the loader layer + ingest_directory end-to-end against real Qdrant.
func TestRAG_E2E_IngestDirectory(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("set QDRANT_URL to run the live ingest-directory E2E")
	}
	dir := t.TempDir()
	files := map[string]string{
		"deploy.md":  "# Deployment\nHow to deploy an application to the production server.",
		"cake.txt":   "A recipe for chocolate cake with butter and sugar.",
		"backup.html": "<h1>Backups</h1><p>Nightly database backups are rotated weekly.</p>",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()
	m := New()
	ctx := context.Background()
	ctx = module.WithAppID(ctx, "e2e-dir")
	ctx = module.WithEmbedder(ctx, onnxEmbedder{mgr: mgr})
	ctx = module.WithModuleConfig(ctx, map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": url},
	})

	_, _ = m.deleteKB(ctx, raw(`{"name":"dir"}`))
	res, _ := m.ingestDirectory(ctx, raw(`{"knowledge_base":"dir","path":`+jsonStr(dir)+`}`))
	if !res.Success {
		t.Fatalf("ingest_directory: %s", res.Error)
	}
	if n, _ := res.Data.(map[string]any)["files"].(int); n != 3 {
		t.Fatalf("ingested %d files, want 3", n)
	}

	res, _ = m.query(ctx, raw(`{"knowledge_base":"dir","query":"how do I deploy to the server","top_k":3}`))
	results, _ := res.Data.(map[string]any)["results"].([]map[string]any)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	top, _ := results[0]["source"].(string)
	t.Logf("top source=%q", top)
	if top != "deploy.md" {
		t.Errorf("top source = %q, want deploy.md", top)
	}
	_, _ = m.deleteKB(ctx, raw(`{"name":"dir"}`))
}

// Config-declared file source → SyncAll (incremental) → query, against
// real Qdrant + real embeddings. Proves the internal sync path.
func TestRAG_E2E_ConfigSources(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("set QDRANT_URL to run the config-sources E2E")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"deploy.md": "How to deploy an application to the production server.",
		"cake.md":   "A chocolate cake recipe with butter.",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	cfg, _ := ParseConfig(map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": url},
		"sources":         []any{map[string]any{"type": "file", "path": dir, "knowledge_base": "cfgsrc"}},
		"auto_index":      map[string]any{"on_start": true},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	_ = be.DeleteKB(context.Background(), "cfgsrc")

	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)
	rep, err := eng.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("syncall: %v", err)
	}
	if rep.Added != 2 {
		t.Fatalf("sync report = %+v, want Added=2", rep)
	}
	hits, err := eng.Query(context.Background(), "cfgsrc", "how do I deploy to the server", 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 || hits[0].Source != "deploy.md" {
		t.Fatalf("top hit = %+v, want deploy.md", hits)
	}
	_ = be.DeleteKB(context.Background(), "cfgsrc")
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
