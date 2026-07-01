//go:build onnx

package backend

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings/loader"
	"github.com/digitornai/digitorn/internal/embeddings/models"
)

// Real cross-encoder proof. Downloads bge-reranker-base int8 (~266 MB).
//
//	ONNXRUNTIME_LIB=bin/onnxruntime.dll DIGITORN_TEST_RERANKER=1 \
//	  go test -tags onnx ./internal/embeddings/backend/ -run TestCrossEncoder -v
func TestCrossEncoder_RerankReal(t *testing.T) {
	if os.Getenv("DIGITORN_TEST_RERANKER") == "" {
		t.Skip("set DIGITORN_TEST_RERANKER=1 to run (downloads bge-reranker-base)")
	}
	spec, ok := models.ResolveReranker("bge-reranker-base")
	if !ok {
		t.Fatal("reranker spec not found")
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".digitorn", "models", spec.ID)
	const modelFile = "model_quantized.onnx"
	files := []loader.File{
		{Name: modelFile, URL: spec.ModelURL(modelFile)},
		{Name: "tokenizer.json", URL: spec.TokenizerURL()},
	}
	if err := loader.Ensure(context.Background(), dir, files, slog.Default()); err != nil {
		t.Fatalf("download: %v", err)
	}
	ce, err := NewONNXCrossEncoder(dir, modelFile, spec)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer ce.Close()

	query := "how do I deploy an application to the production server"
	docs := []string{
		"a step-by-step guide to deploying applications in production",
		"a traditional recipe for molten chocolate cake with butter",
	}
	scores, err := ce.Rerank(context.Background(), query, docs)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	t.Logf("scores: relevant=%.3f irrelevant=%.3f", scores[0], scores[1])
	if scores[0] <= scores[1] {
		t.Errorf("reranker wrong order: relevant %.3f should beat irrelevant %.3f", scores[0], scores[1])
	}
}
