// Command digitorn-background is the standalone background-agents service: it
// listens to channels/triggers and launches agentic sessions by invoking the
// daemon's PUBLIC HTTP API. It is fully isolated — it imports nothing from the
// daemon's server/runtime and the daemon never imports it. Configure via
// DIGITORN_BG_* env vars (SQLite local by default, zero config).
//
// At boot it discovers each app's `tools.modules.channels` config from the app
// bundles, arms the triggers, and wires the webhook + cron adapters; a periodic
// re-scan picks up installs / config changes. Each inbound event flows through
// the channel pipeline and is launched on the daemon over its public API.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/daemonclient"
	"github.com/mbathepaul/digitorn/internal/background/discovery"
	"github.com/mbathepaul/digitorn/internal/background/processor"
	"github.com/mbathepaul/digitorn/internal/background/service"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// inbound wraps the adapter manager with the periodic config re-scan so the
// service can drive both through the single Inbound interface.
type inbound struct {
	mgr   *processor.Manager
	dir   string
	every time.Duration
	log   *slog.Logger
}

func (i *inbound) Handler() http.Handler { return i.mgr.Handler() }

func (i *inbound) Start(ctx context.Context) error {
	go discovery.Rescan(ctx, i.mgr, i.dir, i.every, os.Getenv, i.log)
	return i.mgr.Start(ctx)
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := service.FromEnv()
	client := daemonclient.New(cfg.DaemonURL, cfg.ServiceJWT)

	// Discover channel configs from the app bundles, arm triggers, build adapters,
	// and wire the channel processor (which needs the adapter registry for reply
	// delivery). All from the freshly-opened store.
	build := func(st *store.Store) (service.Setup, error) {
		apps, err := discovery.ScanApps(cfg.AppsDir)
		if err != nil {
			log.Warn("background: apps dir unreadable, no channels armed", "dir", cfg.AppsDir, "err", err.Error())
		}
		plan := discovery.BuildPlan(apps, os.Getenv)
		mgr, reg, err := discovery.Arm(context.Background(), st, plan, log)
		if err != nil {
			return service.Setup{}, err
		}
		proc := processor.New(st, client, reg, nil, log)
		inb := &inbound{mgr: mgr, dir: cfg.AppsDir, every: time.Duration(cfg.RescanSec) * time.Second, log: log}
		return service.Setup{Processor: proc, Inbound: inb}, nil
	}

	svc, err := service.New(cfg, build, log)
	if err != nil {
		log.Error("background: init failed", "err", err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := svc.Run(ctx); err != nil {
		log.Error("background: run failed", "err", err.Error())
		os.Exit(1)
	}
}
