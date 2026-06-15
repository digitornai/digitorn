// Command digitorn-worker is the generic subprocess worker binary. It
// hosts any Digitorn module declared in the DIGITORN_WORKER_MODULES env
// var and exposes them over gRPC via internal/module/service.
//
// The daemon spawns one of these per worker pool, passing :
//
//	DIGITORN_WORKER_SECRET=<HMAC>            // worker.ReadEnvHandshake
//	DIGITORN_WORKER_KIND=<pool-id>           // worker.ReadEnvHandshake
//	DIGITORN_WORKER_MODULES=shell,filesystem // which modules to host
//	DIGITORN_MODULE_SHELL_CONFIG={"workdir":"/tmp"} // per-module cfg
//
// Adding support for a new module means importing its package below
// (side-effect, triggers init() → module.MustRegister) ; no other
// code change. The list of modules an operator chooses to actually
// run lives in the daemon config (workers.pools[].modules).
package main

import (
	"fmt"
	"os"

	_ "go.uber.org/automaxprocs"

	"github.com/mbathepaul/digitorn/internal/module/worker"

	// Built-in modules. Each side-effect import registers the module
	// in pkg/module.Default ; the runner picks the right ones at
	// startup based on DIGITORN_WORKER_MODULES. Importing here does
	// NOT mean the worker hosts ALL of them — it just makes them
	// available for the runner to choose from.
	_ "github.com/mbathepaul/digitorn/internal/modules/bash"
	_ "github.com/mbathepaul/digitorn/internal/modules/database"
	_ "github.com/mbathepaul/digitorn/internal/modules/filesystem"
	_ "github.com/mbathepaul/digitorn/internal/modules/lsp"
	_ "github.com/mbathepaul/digitorn/internal/modules/mcp"
	_ "github.com/mbathepaul/digitorn/internal/modules/pieces"
	_ "github.com/mbathepaul/digitorn/internal/modules/rag"
	_ "github.com/mbathepaul/digitorn/internal/modules/web"
	_ "github.com/mbathepaul/digitorn/internal/modules/workspace"
)

func main() {
	if err := worker.Run(worker.Defaults()); err != nil {
		fmt.Fprintf(os.Stderr, "digitorn-worker: %v\n", err)
		os.Exit(1)
	}
}
