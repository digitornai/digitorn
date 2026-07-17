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

	"github.com/digitornai/digitorn/internal/background/adapter/cron"
	"github.com/digitornai/digitorn/internal/background/adapter/pieces"
	"github.com/digitornai/digitorn/internal/background/adapter/webhook"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
	"github.com/digitornai/digitorn/internal/background/discovery"
	"github.com/digitornai/digitorn/internal/background/processor"
	"github.com/digitornai/digitorn/internal/background/service"
	"github.com/digitornai/digitorn/internal/background/store"
	"github.com/digitornai/digitorn/internal/background/userauth"
)

type inbound struct{ mgr *processor.Manager }

func (i *inbound) Handler() http.Handler           { return i.mgr.Handler() }
func (i *inbound) Start(ctx context.Context) error { return i.mgr.Start(ctx) }

var channelRuntimeAdapters = map[string]bool{
	"discord": true, "telegram": true, "rss": true, "whatsapp": true,
}

func loadWebhooksFromDB(ctx context.Context, st *store.Store) ([]webhook.Provider, error) {
	triggers, err := st.AllTriggers(ctx, "", true)
	if err != nil {
		return nil, err
	}
	var out []webhook.Provider
	for _, t := range triggers {
		if t.Adapter != "webhook" || t.ConfigJSON == "" {
			continue
		}
		var spec processor.TriggerSpec
		if err := json.Unmarshal([]byte(t.ConfigJSON), &spec); err != nil || len(spec.Config) == 0 {
			continue
		}
		p := discovery.WebhookProviderFromConfig(t.Provider, spec.Config)
		if p.Path == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func mergeWebhookProviders(yaml, db []webhook.Provider) []webhook.Provider {
	byPath := make(map[string]webhook.Provider, len(yaml)+len(db))
	order := make([]string, 0, len(yaml)+len(db))
	for _, p := range append(append([]webhook.Provider{}, yaml...), db...) {
		if p.Path == "" {
			continue
		}
		if _, seen := byPath[p.Path]; !seen {
			order = append(order, p.Path)
		}
		byPath[p.Path] = p
	}
	out := make([]webhook.Provider, 0, len(order))
	for _, path := range order {
		out = append(out, byPath[path])
	}
	return out
}

func rearmFunc(client *daemonclient.Client, st *store.Store, mgr *processor.Manager, ca *cron.Adapter, pa *pieces.Adapter, wa *webhook.Adapter, umgr *userauth.Manager) func(context.Context, service.CreateTriggerRequest) (store.Trigger, error) {
	return func(ctx context.Context, req service.CreateTriggerRequest) (store.Trigger, error) {
		if umgr != nil && req.Owner != "" && req.RefreshToken != "" {
			_ = umgr.Save(ctx, req.Owner, req.RefreshToken)
		}
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
		if req.Owner != "" {
			activation.Owner = req.Owner
		}

		spec := processor.TriggerSpec{
			AppID: req.AppID, Provider: req.Provider, Adapter: req.Adapter,
			DefaultAgent: req.Agent, Schedule: req.Schedule,
			Activation: activation, Config: req.Config,
		}

		switch req.Adapter {
		case "webhook":
			if wa == nil {
				return store.Trigger{}, fmt.Errorf("webhook adapter not initialized")
			}
			p := discovery.WebhookProviderFromConfig(req.Provider, req.Config)
			if err := wa.Arm(p); err != nil {
				return store.Trigger{}, err
			}
			id, err := mgr.Arm(ctx, spec)
			if err != nil {
				return store.Trigger{}, err
			}
			return st.GetTrigger(ctx, id)

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
			var catchUp time.Time
			if runs, rerr := st.ListRuns(ctx, store.RunFilter{TriggerID: id, Limit: 1}); rerr == nil && len(runs) == 1 {
				catchUp = runs[0].StartedAt
			}
			ca.Arm(cron.Provider{Name: req.Provider, Schedule: sched, CatchUpFrom: catchUp})
			return st.GetTrigger(ctx, id)

		case "pieces":
			if pa == nil {
				return store.Trigger{}, fmt.Errorf("pieces adapter not initialized")
			}
			p, err := piecesProviderFromConfig(ctx, client, req)
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
			if channelRuntimeAdapters[req.Adapter] {
				id, err := mgr.Arm(ctx, spec)
				if err != nil {
					return store.Trigger{}, err
				}
				return st.GetTrigger(ctx, id)
			}
			return store.Trigger{}, fmt.Errorf("adapter %q cannot be armed at runtime (supported: cron, pieces, channels)", req.Adapter)
		}
	}
}

func disarmFunc(ca *cron.Adapter) func(context.Context, store.Trigger) error {
	return func(_ context.Context, t store.Trigger) error {
		if t.Adapter == "cron" {
			ca.Disarm(t.Provider)
		}
		return nil
	}
}

func resolvePiecesAuth(ctx context.Context, client *daemonclient.Client, owner string, cfg map[string]any, piece string) any {
	installed := ""
	switch v := cfg["auth_from_installed"].(type) {
	case string:
		installed = v
	case bool:
		if v {
			installed = piece
		}
	}
	if installed != "" && client != nil {
		if wire, err := client.PieceAuth(ctx, owner, installed); err == nil && len(wire) > 0 {
			return wire
		}
	}
	a, ok := cfg["auth"]
	if !ok {
		return nil
	}
	am, ok := a.(map[string]any)
	if !ok {
		return a
	}
	resolved := make(map[string]any, len(am))
	for k, v := range am {
		if s, ok := v.(string); ok {
			resolved[k] = os.ExpandEnv(s)
		} else {
			resolved[k] = v
		}
	}
	return resolved
}

func piecesProviderFromConfig(ctx context.Context, client *daemonclient.Client, req service.CreateTriggerRequest) (pieces.Provider, error) {
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

	auth := resolvePiecesAuth(ctx, client, req.Owner, cfg, piece)

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

func loadPiecesFromDB(ctx context.Context, client *daemonclient.Client, st *store.Store) ([]pieces.Provider, error) {
	triggers, err := st.AllTriggers(ctx, "", true)
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
		owner, _ := cfg["owner"].(string)
		auth := resolvePiecesAuth(ctx, client, owner, cfg, piece)
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
		piecesProviders, err := loadPiecesFromDB(context.Background(), client, st)
		if err != nil {
			log.Warn("background: could not load pieces triggers from DB", "err", err.Error())
		}

		pa := discovery.NewPiecesAdapter(piecesProviders, st, log)
		ca := cron.New(nil)

		// Per-user token manager: keeps a fresh access token per owner (refresh
		// against the auth service) so background turns carry a real UserJWT for
		// the LLM gateway. Shares the service's DB; feeds the daemonclient.
		ustore := userauth.NewStore(st.DB())
		if merr := ustore.Migrate(); merr != nil {
			log.Warn("background: user-token store migrate failed", "err", merr.Error())
		}
		umgr := userauth.NewManager(ustore, userauth.NewClient(cfg.AuthURL))
		client.SetUserTokenProvider(umgr.Token)

		reg := discovery.NewBaseRegistry(ca, pa)

		// Channels (discord/telegram/…) are discovered from the installed app
		// YAMLs and armed as persistent listeners — a separate path from the
		// DB-scheduled cron/pieces above. Register their adapters into the SAME
		// registry before the manager is built so Manager.Start launches them.
		// Secrets ({{secret.X}}) resolve from env for now (DIGITORN_BG_SECRET_*).
		var channelPlan discovery.Plan
		if apps, serr := discovery.ScanApps(cfg.AppsDir); serr != nil {
			log.Warn("background: apps dir unreadable, no channels armed", "dir", cfg.AppsDir, "err", serr.Error())
		} else {
			// {{secret.X}} resolves from the daemon's per-app secret store first
			// (the UI-pasted token), falling back to env vars.
			secretFn := func(appID, key string) string {
				v, _ := client.AppChannelSecret(context.Background(), appID, key)
				return v
			}
			channelPlan = discovery.BuildPlan(apps, os.Getenv, secretFn)
			discovery.RegisterChannelAdapters(reg, channelPlan, st, log)
		}

		// One shared webhook adapter: YAML-scanned providers + DB-persisted ones
		// (pushed at app install), always registered — even empty — so a webhook
		// pushed later arms a live route on THIS instance without a restart.
		dbHooks, werr := loadWebhooksFromDB(context.Background(), st)
		if werr != nil {
			log.Warn("background: could not load webhook triggers from DB", "err", werr.Error())
		}
		wa := webhook.New(mergeWebhookProviders(channelPlan.Webhooks, dbHooks))
		reg.Register(wa)

		mgr, err := discovery.ArmFromDB(context.Background(), st, reg, log)
		if err != nil {
			return service.Setup{}, err
		}

		// Arm the channel triggers on the same manager (alongside the DB ones).
		for _, tr := range discovery.ChannelTriggers(channelPlan) {
			if _, aerr := mgr.Arm(context.Background(), tr); aerr != nil {
				log.Warn("background: arm channel trigger failed", "provider", tr.Provider, "adapter", tr.Adapter, "err", aerr.Error())
			}
		}

		proc := processor.New(st, client, reg, nil, log)
		return service.Setup{
			Processor: proc,
			Inbound:   &inbound{mgr: mgr},
			Rearm:     rearmFunc(client, st, mgr, ca, pa, wa, umgr),
			Disarm:    disarmFunc(ca),
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
