// digitorn-worker-embeddings is the subprocess that hosts the
// semantic-search / RAG embeddings service. It serves MULTIPLE models
// from one process : the doc-default paraphrase-multilingual-MiniLM-
// L12-v2 (384) plus any other catalogue model an app requests (bge-m3,
// mpnet, …). Each model is loaded on first use and cached.
//
// ENV-driven config :
//
//	DIGITORN_WORKER_SECRET    — handshake secret (set by Manager)
//	DIGITORN_WORKER_KIND      — should be "embeddings"
//	DIGITORN_WORKER_BIND      — bind addr ("127.0.0.1:0" default)
//	DIGITORN_EMBED_BACKEND    — "onnx" | "deterministic" | auto (default)
//	DIGITORN_EMBED_MODELS_DIR — base dir for per-model subdirs
//	                            (default ~/.digitorn/models)
//	DIGITORN_EMBED_QUANTIZED  — "1" to load the int8 graph
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

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/worker"
)

func main() {
	hs, err := worker.ReadEnvHandshake()
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: bad handshake: %v\n", err)
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mgr := embeddings.NewManager(modelsBaseDir(), embedMode(), quantized(), log)
	defer mgr.Close()

	// Pre-warm the default model : load it now so the worker reports a
	// real readiness and, in strict ONNX mode, fails fast instead of
	// erroring on the first request.
	model, dim, err := mgr.DefaultModel(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: default model init: %v\n", err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "worker-embeddings: mode=%s default-model=%s dim=%d\n", embedMode(), model, dim)

	svc := embeddings.NewServer(mgr)

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

// embedMode maps DIGITORN_EMBED_BACKEND to a manager Mode.
func embedMode() embeddings.Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_EMBED_BACKEND"))) {
	case "deterministic", "fallback", "mock":
		return embeddings.ModeDeterministic
	case "onnx":
		return embeddings.ModeONNX
	default:
		return embeddings.ModeAuto
	}
}

// modelsBaseDir resolves the parent directory of the per-model dirs.
// Prefers the new DIGITORN_EMBED_MODELS_DIR ; for backward compatibility
// falls back to the parent of the legacy DIGITORN_EMBED_MODEL_DIR (which
// historically pointed at the single model's own directory).
func modelsBaseDir() string {
	if base := strings.TrimSpace(os.Getenv("DIGITORN_EMBED_MODELS_DIR")); base != "" {
		return base
	}
	if legacy := strings.TrimSpace(os.Getenv("DIGITORN_EMBED_MODEL_DIR")); legacy != "" {
		return filepath.Dir(legacy)
	}
	return "" // manager defaults to ~/.digitorn/models
}

// quantized reports whether to load the int8 graph.
func quantized() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_EMBED_QUANTIZED"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
