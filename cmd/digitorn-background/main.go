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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter/cron"
	"github.com/mbathepaul/digitorn/internal/background/channels"
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

// rearmFunc builds the POST /ops/triggers handler's hook: it programs a cron at
// runtime by persisting + routing the trigger (mgr.Arm) and arming the live cron
// adapter (cron.Adapter.Arm) — no restart. Only cron is live-armable; other
// adapters are loop/handler-bound at boot and must go through the app YAML.
func rearmFunc(st *store.Store, mgr *processor.Manager, ca *cron.Adapter) func(context.Context, service.CreateTriggerRequest) (store.Trigger, error) {
	return func(ctx context.Context, req service.CreateTriggerRequest) (store.Trigger, error) {
		if req.Adapter != "cron" {
			return store.Trigger{}, fmt.Errorf("runtime arming supports only cron (got %q); add other adapters to the app YAML and restart", req.Adapter)
		}
		sched, err := cron.Parse(req.Schedule)
		if err != nil {
			return store.Trigger{}, fmt.Errorf("invalid cron schedule %q: %w", req.Schedule, err)
		}
		spec := processor.TriggerSpec{
			AppID: req.AppID, Provider: req.Provider, Adapter: "cron",
			DefaultAgent: req.Agent, Schedule: req.Schedule,
			Activation: channels.ActivationConfig{
				Agent:   req.Agent,
				Message: req.Message,
				Owner:   req.Owner,   // the woken session runs AS this user
				Context: req.Context, // injected into the agent at wake
				Session:     orDefault(req.Session, channels.SessionPerEvent),
				Reply:       orDefault(req.Reply, channels.ReplyAuto),
				Reports:     req.Reports, // opt-in : dated downloadable output folder per fire
				Attachments: req.Attachments, // input blobs (e.g. a CV) carried to every fire
			},
		}
		// A schedule binds a specific session to wake; a plain trigger is a channel.
		arm := mgr.Arm
		if req.Kind == "schedule" {
			arm = mgr.ArmSchedule
		}
		id, err := arm(ctx, spec) // persist (with schedule) + register the provider→trigger route
		if err != nil {
			return store.Trigger{}, err
		}
		ca.Arm(cron.Provider{Name: req.Provider, Schedule: sched}) // fire live, no restart
		return st.GetTrigger(ctx, id)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := service.FromEnv()
	client := daemonclient.New(cfg.DaemonURL, cfg.ServiceJWT)

	// Fail-early credential self-check : a wake of a USER-owned session impersonates
	// that user (X-Act-As-User), which the daemon authorises only for a token with
	// the impersonation grant. Catch a mis-scoped service token at boot with a clear
	// warning instead of a confusing 403 on every fire.
	switch {
	case cfg.ServiceJWT == "":
		log.Info("background: no service JWT (DIGITORN_BG_SERVICE_JWT) set — OK only if the daemon runs with auth disabled (dev); in production, waking user-owned sessions requires a service token")
	case !daemonclient.CanImpersonate(cfg.ServiceJWT):
		log.Warn("background: service JWT lacks the impersonation grant — wakes of user-owned sessions will be rejected (403). Use a dedicated SERVICE token (role \"service\" or permission \"sessions:impersonate\"), not a plain user token")
	}

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
		// Ensure a cron adapter exists even when no app declares a cron, so the ops
		// API can program one at runtime. Registered before Start, so the manager
		// launches it. (No-op when discovery already registered one.)
		if reg.Get("cron") == nil {
			reg.Register(cron.New(nil))
		}
		ca, _ := reg.Get("cron").(*cron.Adapter)
		proc := processor.New(st, client, reg, nil, log)
		inb := &inbound{mgr: mgr, dir: cfg.AppsDir, every: time.Duration(cfg.RescanSec) * time.Second, log: log}
		return service.Setup{Processor: proc, Inbound: inb, Rearm: rearmFunc(st, mgr, ca)}, nil
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
