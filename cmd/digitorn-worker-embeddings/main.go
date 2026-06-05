// digitorn-worker-embeddings is the subprocess that hosts the
// semantic-search embeddings module. Per docs-site/language/04-
// tools.md "Semantic search", the production daemon uses
// paraphrase-multilingual-MiniLM-L12-v2 (384 dims) for the tool
// index ; this worker is the inference endpoint.
//
// ENV-driven config :
//
//	DIGITORN_WORKER_SECRET   — handshake secret (set by Manager)
//	DIGITORN_WORKER_KIND     — should be "embeddings"
//	DIGITORN_WORKER_BIND     — bind addr ("127.0.0.1:0" default)
//	DIGITORN_EMBED_BACKEND   — "onnx" | "deterministic"
//	                           default : onnx if available, else deterministic
//	DIGITORN_EMBED_MODEL_DIR — model directory (default ~/.digitorn/models/<model>)
//
// Build :
//
//	go build ./cmd/digitorn-worker-embeddings              # deterministic only
//	go build -tags onnx ./cmd/digitorn-worker-embeddings   # production (ONNX)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"

	"github.com/mbathepaul/digitorn/internal/embeddings"
	"github.com/mbathepaul/digitorn/internal/embeddings/backend"
	"github.com/mbathepaul/digitorn/internal/embeddings/loader"
	"github.com/mbathepaul/digitorn/internal/worker"
)

func main() {
	hs, err := worker.ReadEnvHandshake()
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: bad handshake: %v\n", err)
		os.Exit(2)
	}

	be, modelInfo, err := selectBackend()
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: backend init: %v\n", err)
		os.Exit(3)
	}
	defer be.Close()
	fmt.Fprintf(os.Stderr, "worker-embeddings: backend=%s model=%s dim=%d\n",
		modelInfo.kind, be.Model(), be.Dimension())

	svc := embeddings.NewServer(be)

	bindAddr := os.Getenv("DIGITORN_WORKER_BIND")
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}

	if err := worker.Run(worker.ServerConfig{
		Handshake: hs,
		BindAddr:  bindAddr,
		Register: func(s *grpc.Server) {
			embeddings.RegisterService(s, svc)
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: run: %v\n", err)
		os.Exit(1)
	}
}

// backendChoice records what was selected, for the startup log.
type backendChoice struct{ kind string }

// selectBackend picks an inference backend per DIGITORN_EMBED_BACKEND
// (or auto-detects). Returns (backend, choice, error).
//
//   - "onnx" : try to load the ONNX runtime ; error if not built
//     with -tags onnx.
//   - "deterministic" : the pure-Go hashing fallback.
//   - auto (default) : try onnx first, fall back to deterministic
//     when the build doesn't include the tag.
//
// In all cases the dimension is 384.
func selectBackend() (backend.Backend, backendChoice, error) {
	choice := strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_EMBED_BACKEND")))
	modelDir := os.Getenv("DIGITORN_EMBED_MODEL_DIR")
	if modelDir == "" {
		home, _ := os.UserHomeDir()
		modelDir = filepath.Join(home, ".digitorn", "models", embeddings.DefaultModel)
	}

	// model_quantized.onnx (int8, ~4x smaller/faster) when requested,
	// else the full-precision model.onnx.
	modelFile := "model.onnx"
	if q := strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_EMBED_QUANTIZED"))); q == "1" || q == "true" || q == "yes" {
		modelFile = "model_quantized.onnx"
	}

	switch choice {
	case "deterministic", "fallback", "mock":
		return backend.NewDeterministic(embeddings.EmbeddingDim), backendChoice{kind: "deterministic"}, nil
	case "onnx":
		ensureModel(modelDir, modelFile)
		be, err := backend.NewONNXWithFile(modelDir, modelFile)
		if err != nil {
			return nil, backendChoice{}, err
		}
		return be, backendChoice{kind: "onnx:" + modelFile}, nil
	}

	// Auto : try onnx, gracefully fall back.
	ensureModel(modelDir, modelFile)
	if be, err := backend.NewONNXWithFile(modelDir, modelFile); err == nil {
		return be, backendChoice{kind: "onnx:" + modelFile}, nil
	}
	return backend.NewDeterministic(embeddings.EmbeddingDim), backendChoice{kind: "deterministic"}, nil
}

// ensureModel downloads the chosen model graph + tokenizer into modelDir
// on first start (no-op when the files are already present). A download
// failure is non-fatal here : NewONNX reports the precise missing-file
// error, and auto mode then degrades to the deterministic backend.
func ensureModel(modelDir, modelFile string) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := loader.Ensure(context.Background(), modelDir, loader.Files(modelFile), log); err != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: model fetch: %v\n", err)
	}
}
