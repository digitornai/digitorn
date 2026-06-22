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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter/cron"
	"github.com/mbathepaul/digitorn/internal/background/adapter/pieces"
	"github.com/mbathepaul/digitorn/internal/background/channels"
	"github.com/mbathepaul/digitorn/internal/background/daemonclient"
	"github.com/mbathepaul/digitorn/internal/background/discovery"
	"github.com/mbathepaul/digitorn/internal/background/processor"
	"github.com/mbathepaul/digitorn/internal/background/service"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

type inbound struct{ mgr *processor.Manager }

func (i *inbound) Handler() http.Handler         { return i.mgr.Handler() }
func (i *inbound) Start(ctx context.Context) error { return i.mgr.Start(ctx) }

func rearmFunc(st *store.Store, mgr *processor.Manager, ca *cron.Adapter, pa *pieces.Adapter) func(context.Context, service.CreateTriggerRequest) (store.Trigger, error) {
	return func(ctx context.Context, req service.CreateTriggerRequest) (store.Trigger, error) {
		activation := channels.ActivationConfig{
			Agent:       req.Agent,
			Message:     req.Message,
			Owner:       req.Owner,
			Context:     req.Context,
			Session:     orDefault(req.Session, channels.SessionPerEvent),
			Reply:       orDefault(req.Reply, channels.ReplyAuto),
			Reports:     req.Reports,
			Attachments: req.Attachments,
		}
		if req.Activation != nil {
			activation = *req.Activation
		}

		spec := processor.TriggerSpec{
			AppID: req.AppID, Provider: req.Provider, Adapter: req.Adapter,
			DefaultAgent: req.Agent, Schedule: req.Schedule,
			Activation: activation,
		}

		switch req.Adapter {
		case "cron":
			sched, err := cron.Parse(req.Schedule)
			if err != nil {
				return store.Trigger{}, fmt.Errorf("invalid cron schedule %q: %w", req.Schedule, err)
			}
			arm := mgr.Arm
			if req.Kind == "schedule" {
				arm = mgr.ArmSchedule
			}
			id, err := arm(ctx, spec)
			if err != nil {
				return store.Trigger{}, err
			}
			ca.Arm(cron.Provider{Name: req.Provider, Schedule: sched})
			return st.GetTrigger(ctx, id)

		case "pieces":
			if pa == nil {
				return store.Trigger{}, fmt.Errorf("pieces adapter not initialized")
			}
			p, err := piecesProviderFromConfig(req)
			if err != nil {
				return store.Trigger{}, err
			}
			id, err := mgr.Arm(ctx, spec)
			if err != nil {
				return store.Trigger{}, err
			}
			p.CursorKey = id
			pa.Arm(p)
			return st.GetTrigger(ctx, id)

		default:
			return store.Trigger{}, fmt.Errorf("adapter %q cannot be armed at runtime (supported: cron, pieces)", req.Adapter)
		}
	}
}

func piecesProviderFromConfig(req service.CreateTriggerRequest) (pieces.Provider, error) {
	cfg := req.Config
	if cfg == nil {
		return pieces.Provider{}, fmt.Errorf("pieces trigger requires config")
	}
	piece, _ := cfg["piece"].(string)
	trigger, _ := cfg["trigger"].(string)
	if piece == "" || trigger == "" {
		return pieces.Provider{}, fmt.Errorf("pieces config requires 'piece' and 'trigger'")
	}
	url, _ := cfg["trigger_url"].(string)
	if url == "" {
		url = "http://127.0.0.1:9234"
	}
	interval := 60 * time.Second
	if sec, ok := cfg["interval"].(float64); ok && sec > 0 {
		interval = time.Duration(sec) * time.Second
	}

	// Serialize auth back to map for the provider.
	var auth any
	if a, ok := cfg["auth"]; ok {
		// Re-resolve env placeholders if auth is a string map.
		if am, ok := a.(map[string]any); ok {
			resolved := make(map[string]any, len(am))
			for k, v := range am {
				if s, ok := v.(string); ok {
					resolved[k] = os.ExpandEnv(s)
				} else {
					resolved[k] = v
				}
			}
			auth = resolved
		} else {
			auth = a
		}
	}

	var props map[string]any
	if p, ok := cfg["props"].(map[string]any); ok {
		props = p
	}

	return pieces.Provider{
		Name:       req.Provider,
		TriggerURL: url,
		Piece:      piece,
		Trigger:    trigger,
		Auth:       auth,
		Props:      props,
		Interval:   interval,
	}, nil
}

func loadPiecesFromDB(st *store.Store) ([]pieces.Provider, error) {
	triggers, err := st.AllTriggers(context.Background(), "", true)
	if err != nil {
		return nil, err
	}
	var out []pieces.Provider
	for _, t := range triggers {
		if t.Adapter != "pieces" || t.ConfigJSON == "" {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(t.ConfigJSON), &cfg); err != nil {
			continue
		}
		piece, _ := cfg["piece"].(string)
		trigger, _ := cfg["trigger"].(string)
		if piece == "" || trigger == "" {
			continue
		}
		url, _ := cfg["trigger_url"].(string)
		if url == "" {
			url = "http://127.0.0.1:9234"
		}
		interval := 60 * time.Second
		if sec, ok := cfg["interval"].(float64); ok && sec > 0 {
			interval = time.Duration(sec) * time.Second
		}
		var auth any
		if a, ok := cfg["auth"]; ok {
			if am, ok := a.(map[string]any); ok {
				resolved := make(map[string]any, len(am))
				for k, v := range am {
					if s, ok := v.(string); ok {
						resolved[k] = os.ExpandEnv(s)
					} else {
						resolved[k] = v
					}
				}
				auth = resolved
			} else {
				auth = a
			}
		}
		var props map[string]any
		if p, ok := cfg["props"].(map[string]any); ok {
			props = p
		}
		out = append(out, pieces.Provider{
			Name:       t.Provider,
			TriggerURL: url,
			Piece:      piece,
			Trigger:    trigger,
			Auth:       auth,
			Props:      props,
			CursorKey:  t.ID,
			Interval:   interval,
		})
	}
	return out, nil
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

	build := func(st *store.Store) (service.Setup, error) {
		// Boot from DB: reload pieces triggers persisted from previous runs.
		// The daemon pushes triggers via POST /ops/triggers on app install —
		// no filesystem scanning needed.
		piecesProviders, err := loadPiecesFromDB(st)
		if err != nil {
			log.Warn("background: could not load pieces triggers from DB", "err", err.Error())
		}

		pa := discovery.NewPiecesAdapter(piecesProviders, st, log)
		ca := cron.New(nil)

		reg := discovery.NewBaseRegistry(ca, pa)
		mgr, err := discovery.ArmFromDB(context.Background(), st, reg, log)
		if err != nil {
			return service.Setup{}, err
		}

		proc := processor.New(st, client, reg, nil, log)
		return service.Setup{
			Processor: proc,
			Inbound:   &inbound{mgr: mgr},
			Rearm:     rearmFunc(st, mgr, ca, pa),
		}, nil
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
