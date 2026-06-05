package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/grpc"

	"github.com/mbathepaul/digitorn/internal/core/servicebus"
	"github.com/mbathepaul/digitorn/internal/module/service"
	workerfw "github.com/mbathepaul/digitorn/internal/worker"
	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

// EnvModules is the env var the daemon sets when spawning a worker
// process to indicate which modules to host. Format : comma-separated
// module IDs, e.g. "shell,filesystem,lsp". Empty / unset → worker
// hosts nothing (still serves Manifests with empty list).
const EnvModules = "DIGITORN_WORKER_MODULES"

// EnvModuleConfigPrefix is the prefix for per-module config env vars.
// Example : DIGITORN_MODULE_SHELL_CONFIG='{"workdir":"/tmp"}'. The
// value is the JSON config that module.Init receives.
const EnvModuleConfigPrefix = "DIGITORN_MODULE_"
const EnvModuleConfigSuffix = "_CONFIG"

// Options configures the worker runner. Most callers use Defaults().
type Options struct {
	// Registry resolves module IDs to factories. Defaults to
	// pkg/module.Default, which means : any module package the
	// worker binary imports via `_ "internal/modules/<id>"` is
	// automatically resolvable.
	Registry *pkgmodule.Registry

	// Logger is used for startup + per-module errors. Defaults
	// to slog.Default. Workers run as subprocesses ; logging on
	// stderr is captured by the parent worker.Manager.
	Logger *slog.Logger
}

// Defaults returns a runner Options pre-wired with the global registry
// and slog.Default.
func Defaults() Options {
	return Options{
		Registry: pkgmodule.Default,
		Logger:   slog.Default(),
	}
}

// Run is the entry point a worker binary calls in main(). It :
//  1. Reads the handshake (secret + kind) from env vars.
//  2. Reads DIGITORN_WORKER_MODULES, instantiates each module from
//     the registry, Init's it with its per-module config env var,
//     Start's it, and registers it in a local servicebus.Bus.
//  3. Starts the gRPC server via internal/worker.Run, exposing the
//     generic ModuleService on the OS-assigned port.
//
// Run blocks until SIGTERM/SIGINT or the gRPC server errors out.
//
// On any startup failure (handshake missing, module not in registry,
// Init/Start error) Run returns the error and the binary exits non-
// zero. The framework's restart policy (configured at Spawn time)
// decides whether the supervisor retries.
func Run(opts Options) error {
	if opts.Registry == nil {
		opts.Registry = pkgmodule.Default
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	log := opts.Logger

	hs, err := workerfw.ReadEnvHandshake()
	if err != nil {
		return fmt.Errorf("worker: handshake: %w", err)
	}

	modIDs := parseModulesEnv(os.Getenv(EnvModules))
	log.Info("worker starting",
		slog.String("kind", string(hs.Kind)),
		slog.Int("modules", len(modIDs)),
		slog.String("module_ids", strings.Join(modIDs, ",")),
	)

	bus, err := startModules(opts, modIDs)
	if err != nil {
		return fmt.Errorf("worker: start modules: %w", err)
	}

	// workerID = "<kind>#<pid>". The daemon already knows the kind
	// from its Spec ; the pid disambiguates instances on the wire.
	workerID := fmt.Sprintf("%s#%d", hs.Kind, os.Getpid())
	svc := newModuleService(bus, workerID)

	return workerfw.Run(workerfw.ServerConfig{
		Handshake: hs,
		BindAddr:  workerfw.BindAddrFromEnv(),
		Register: func(s *grpc.Server) {
			service.RegisterService(s, svc)
		},
	})
}

// startModules instantiates and starts every module in ids, registering
// each in a fresh servicebus.Bus. Failures are FATAL : a worker that
// can't start one of its declared modules has no business serving.
func startModules(opts Options, ids []string) (*servicebus.Bus, error) {
	bus := servicebus.New()
	ctx := context.Background()
	for _, id := range ids {
		if !opts.Registry.Has(id) {
			return nil, fmt.Errorf("module %q not in registry (forgot import _ %q in cmd/digitorn-worker ?)",
				id, "internal/modules/"+id)
		}
		mod, err := opts.Registry.Get(id)
		if err != nil {
			return nil, fmt.Errorf("instantiate %s: %w", id, err)
		}
		cfg, err := loadModuleConfig(id)
		if err != nil {
			return nil, fmt.Errorf("decode config for %s: %w", id, err)
		}
		if err := mod.Init(ctx, cfg); err != nil {
			return nil, fmt.Errorf("init %s: %w", id, err)
		}
		if err := mod.Start(ctx); err != nil {
			return nil, fmt.Errorf("start %s: %w", id, err)
		}
		if err := bus.Register(mod); err != nil {
			return nil, fmt.Errorf("register %s: %w", id, err)
		}
		opts.Logger.Info("module started",
			slog.String("module_id", id),
			slog.Int("tools", len(mod.Manifest().Tools)),
		)
	}
	return bus, nil
}

// parseModulesEnv splits a comma-separated module list. Whitespace
// around items is trimmed ; empty items are dropped so the value
// "shell, filesystem ," yields ["shell", "filesystem"].
func parseModulesEnv(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadModuleConfig reads DIGITORN_MODULE_<UPPER>_CONFIG as JSON and
// returns the decoded map. Missing env var → nil config (module uses
// its defaults). Invalid JSON → error so the worker fails to start
// loudly instead of running with a wrong configuration.
func loadModuleConfig(id string) (map[string]any, error) {
	key := EnvModuleConfigPrefix + strings.ToUpper(id) + EnvModuleConfigSuffix
	v := os.Getenv(key)
	if v == "" {
		return nil, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(v), &cfg); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", key, err)
	}
	return cfg, nil
}
