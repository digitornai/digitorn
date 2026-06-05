// digitorn-worker-tokenizer is the subprocess that counts tokens out of the
// daemon process. It hosts a tokencount.Counter (tiktoken for OpenAI families,
// heuristic otherwise — fully offline, no model files) behind the shared
// worker gRPC framework. The daemon's ContextService dispatches the
// between-anchor delta here in the background ; the turn loop never waits on
// it.
//
// ENV-driven config (all set by the worker Manager) :
//
//	DIGITORN_WORKER_SECRET — handshake secret
//	DIGITORN_WORKER_KIND   — "tokenizer"
//	DIGITORN_WORKER_BIND   — bind addr ("127.0.0.1:0" default)
//
// Build : go build ./cmd/digitorn-worker-tokenizer  (pure Go, no tags).
package main

import (
	"fmt"
	"os"

	"google.golang.org/grpc"

	"github.com/mbathepaul/digitorn/internal/runtime/tokencount"
	"github.com/mbathepaul/digitorn/internal/tokenizer"
	"github.com/mbathepaul/digitorn/internal/worker"
)

func main() {
	hs, err := worker.ReadEnvHandshake()
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-tokenizer: bad handshake: %v\n", err)
		os.Exit(2)
	}

	svc := tokenizer.NewServer(tokencount.New())

	bindAddr := os.Getenv("DIGITORN_WORKER_BIND")
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}

	if err := worker.Run(worker.ServerConfig{
		Handshake: hs,
		BindAddr:  bindAddr,
		Register: func(s *grpc.Server) {
			tokenizer.RegisterService(s, svc)
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "worker-tokenizer: run: %v\n", err)
		os.Exit(1)
	}
}
