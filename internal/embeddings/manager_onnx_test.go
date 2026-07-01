//go:build onnx

// Real-ONNX proof for the multi-model manager. Compiled + run only with
// `-tags onnx` and a resolvable onnxruntime shared library
// (ONNXRUNTIME_LIB or one next to the test binary). The default model
// (minilm) must already be on disk under ~/.digitorn/models ; the
// bge-m3 case downloads ~2 GB and is gated behind an env flag.
package embeddings_test

import (
	"context"
	"os"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings"
)

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot // inputs are L2-normalised → dot == cosine
}

// minilm via the shortcut : real ONNX, 384-dim, mean pooling, and a
// semantic sanity check (relevant doc beats irrelevant doc).
func TestManager_ONNX_Minilm(t *testing.T) {
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	query := "comment déployer une application sur le serveur"
	relevant := "guide pas à pas pour le déploiement d'applications en production"
	irrelevant := "recette traditionnelle de gâteau au chocolat fondant"

	vecs, model, dim, err := mgr.Embed(context.Background(), "minilm-l12",
		embeddings.RoleQuery, []string{query, relevant, irrelevant}, true)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if model != "paraphrase-multilingual-MiniLM-L12-v2" {
		t.Errorf("model = %q, want canonical minilm", model)
	}
	if dim != 384 || len(vecs) != 3 || len(vecs[0]) != 384 {
		t.Fatalf("dim=%d vecs=%d width=%d", dim, len(vecs), len(vecs[0]))
	}
	rel := cosine(vecs[0], vecs[1])
	irr := cosine(vecs[0], vecs[2])
	t.Logf("cosine relevant=%.3f irrelevant=%.3f", rel, irr)
	if rel <= irr {
		t.Errorf("semantic order wrong: relevant %.3f should beat irrelevant %.3f", rel, irr)
	}
}

// WordPiece proof : bge-small-en (BERT WordPiece, CLS, 384) — validates
// the WordPiece tokenizer produces correct embeddings (unlocks self-host
// code embedders). Quantized graph (~30MB).
func TestManager_ONNX_WordPiece(t *testing.T) {
	if os.Getenv("DIGITORN_TEST_WORDPIECE") == "" {
		t.Skip("set DIGITORN_TEST_WORDPIECE=1 to run (downloads bge-small-en int8)")
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, true, nil) // quantized
	defer mgr.Close()

	query := "how to deploy an application to the server"
	relevant := "a guide to deploying applications in production"
	irrelevant := "a recipe for chocolate cake"
	vecs, model, dim, err := mgr.Embed(context.Background(), "bge-small",
		embeddings.RoleQuery, []string{query, relevant, irrelevant}, true)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if model != "bge-small-en-v1.5" || dim != 384 {
		t.Fatalf("model=%q dim=%d", model, dim)
	}
	rel, irr := cosine(vecs[0], vecs[1]), cosine(vecs[0], vecs[2])
	t.Logf("wordpiece cosine relevant=%.3f irrelevant=%.3f", rel, irr)
	if rel <= irr {
		t.Errorf("WordPiece semantic order wrong: %.3f vs %.3f", rel, irr)
	}
}

// bge-m3 : a SECOND real model proving routing + 1024-dim + CLS pooling.
// Downloads ~2 GB on first run, so it's opt-in.
func TestManager_ONNX_BGEM3(t *testing.T) {
	if os.Getenv("DIGITORN_TEST_BGE_M3") == "" {
		t.Skip("set DIGITORN_TEST_BGE_M3=1 to run (downloads bge-m3 ONNX)")
	}
	// Quantized graph (int8, ~543 MB, self-contained) keeps the real
	// proof light ; set DIGITORN_TEST_BGE_M3_FP32=1 to exercise the
	// full graph + external-data path instead (~2.2 GB).
	quantized := os.Getenv("DIGITORN_TEST_BGE_M3_FP32") == ""
	mgr := embeddings.NewManager("", embeddings.ModeONNX, quantized, nil)
	defer mgr.Close()

	query := "how do I deploy an application to the server"
	relevant := "a step-by-step guide to deploying applications in production"
	irrelevant := "a traditional recipe for molten chocolate cake"

	vecs, model, dim, err := mgr.Embed(context.Background(), "bge-m3",
		embeddings.RoleQuery, []string{query, relevant, irrelevant}, true)
	if err != nil {
		t.Fatalf("embed bge-m3: %v", err)
	}
	if model != "bge-m3" {
		t.Errorf("model = %q, want bge-m3", model)
	}
	if dim != 1024 || len(vecs[0]) != 1024 {
		t.Fatalf("bge-m3 dim=%d width=%d, want 1024", dim, len(vecs[0]))
	}
	rel := cosine(vecs[0], vecs[1])
	irr := cosine(vecs[0], vecs[2])
	t.Logf("bge-m3 cosine relevant=%.3f irrelevant=%.3f", rel, irr)
	if rel <= irr {
		t.Errorf("bge-m3 semantic order wrong: relevant %.3f vs irrelevant %.3f", rel, irr)
	}
}
