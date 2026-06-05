// digitorn-worker-dummy is a no-op worker used by the framework integration
// tests. It loads the handshake, starts a gRPC server with only the
// standard Health service, and supports a few control envs to simulate
// crash / slow-start scenarios.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/mbathepaul/digitorn/internal/worker"
)

func main() {
	hs, err := worker.ReadEnvHandshake()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dummy: bad handshake: %v\n", err)
		os.Exit(2)
	}

	// Test control : optional pre-bind delay to simulate slow startup.
	if d := os.Getenv("DUMMY_STARTUP_DELAY_MS"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			time.Sleep(time.Duration(n) * time.Millisecond)
		}
	}

	// Test control : optional immediate crash.
	if os.Getenv("DUMMY_CRASH_ON_START") == "1" {
		os.Exit(99)
	}

	// Test control : optional exit-after-N-ms (to test restart loop).
	if d := os.Getenv("DUMMY_EXIT_AFTER_MS"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			go func() {
				time.Sleep(time.Duration(n) * time.Millisecond)
				os.Exit(0)
			}()
		}
	}

	if err := worker.Run(worker.ServerConfig{
		Handshake: hs,
		BindAddr:  worker.BindAddrFromEnv(),
		Register:  nil, // dummy only exposes the Health service
	}); err != nil {
		fmt.Fprintf(os.Stderr, "dummy: run: %v\n", err)
		os.Exit(1)
	}
}
