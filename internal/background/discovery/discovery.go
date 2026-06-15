// Package discovery arms the background service from app configuration: it reads
// each installed app's app.yaml, extracts its `tools.modules.channels.config`
// block, and turns the declared providers into armed triggers + configured
// adapters. It reads the shared apps directory directly (no daemon import, no
// daemon API) so it works even when the daemon is down — the design's resilience
// choice. A periodic re-scan picks up installs / config changes.
package discovery

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
	"github.com/mbathepaul/digitorn/internal/background/adapter/cron"
	"github.com/mbathepaul/digitorn/internal/background/adapter/discord"
	"github.com/mbathepaul/digitorn/internal/background/adapter/rss"
	"github.com/mbathepaul/digitorn/internal/background/adapter/telegram"
	"github.com/mbathepaul/digitorn/internal/background/adapter/webhook"
	"github.com/mbathepaul/digitorn/internal/background/adapter/whatsapp"
	"github.com/mbathepaul/digitorn/internal/background/channels"
	"github.com/mbathepaul/digitorn/internal/background/processor"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// appManifest is the slice of an app.yaml we care about: the app id and the
// channels module config. `tools.modules` is a map keyed by module name, so the
// `channels` key binds directly to our struct (other modules are ignored).
type appManifest struct {
	App struct {
		AppID string `yaml:"app_id"`
	} `yaml:"app"`
	AppID string `yaml:"app_id"` // some manifests put it at top level
	Tools struct {
		Modules struct {
			Channels struct {
				Config channels.ModuleConfig `yaml:"config"`
			} `yaml:"channels"`
		} `yaml:"modules"`
	} `yaml:"tools"`
}

// AppChannels is one app's resolved channels config.
type AppChannels struct {
	AppID  string
	Config channels.ModuleConfig
}

// ScanApps reads <dir>/<app>/app.yaml for every app, returning those that declare
// at least one channel provider. Unreadable / malformed manifests are skipped
// (a bad app never breaks discovery of the others).
func ScanApps(dir string) ([]AppChannels, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []AppChannels
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "app.yaml"))
		if err != nil {
			continue
		}
		var m appManifest
		if yaml.Unmarshal(data, &m) != nil {
			continue
		}
		cfg := m.Tools.Modules.Channels.Config
		if len(cfg.Providers) == 0 {
			continue
		}
		cfg.Normalize()
		appID := firstNonEmpty(m.App.AppID, m.AppID, e.Name())
		out = append(out, AppChannels{AppID: appID, Config: cfg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppID < out[j].AppID })
	return out, nil
}

// Plan is the fully-resolved arming plan: durable triggers + the adapter
// providers that produce their events.
type Plan struct {
	Triggers  []processor.TriggerSpec
	Webhooks  []webhook.Provider
	Crons     []cron.Provider
	Feeds     []rss.Provider
	Telegrams []telegram.Provider
	WhatsApps []whatsapp.Provider
	Discords  []discord.Provider
	Warnings  []string // non-fatal per-provider issues (bad schedule, unknown adapter)
}

// BuildPlan turns discovered app channels into an arming plan. env resolves
// {{env.X}} / {{secret.X}} placeholders in adapter config values (so secrets
// stay out of the manifest). A provider with a fatal adapter-config error is
// skipped with a warning — never aborts the whole plan.
func BuildPlan(apps []AppChannels, env func(string) string) Plan {
	if env == nil {
		env = func(string) string { return "" }
	}
	var p Plan
	for _, app := range apps {
		for _, name := range sortedKeys(app.Config.Providers) {
			pc := app.Config.Providers[name]
			if !pc.IsEnabled() {
				continue
			}
			spec := processor.TriggerSpec{
				AppID:        app.AppID,
				Provider:     name,
				Adapter:      pc.Adapter,
				DefaultAgent: app.Config.DefaultAgent,
				SecretFilter: app.Config.FilterSecrets(),
				Activation:   pc.Activation,
			}
			switch pc.Adapter {
			case "webhook":
				p.Webhooks = append(p.Webhooks, webhookProvider(name, pc.Config, env))
			case "cron":
				if cp, ok := cronProvider(name, pc.Config, &p); ok {
					p.Crons = append(p.Crons, cp)
					spec.Schedule = cfgStr(pc.Config, "schedule", env) // for ops next_run
				}
			case "rss":
				p.Feeds = append(p.Feeds, rssProvider(app.AppID, name, pc.Config, env))
			case "telegram":
				p.Telegrams = append(p.Telegrams, telegramProvider(app.AppID, name, pc.Config, env))
			case "whatsapp":
				p.WhatsApps = append(p.WhatsApps, whatsappProvider(name, pc.Config, env))
			case "discord":
				p.Discords = append(p.Discords, discordProvider(name, pc.Config, env))
			default:
				p.Warnings = append(p.Warnings, "provider "+name+": adapter "+pc.Adapter+" not wired (V1: webhook|cron|rss|telegram|whatsapp|discord)")
			}
			p.Triggers = append(p.Triggers, spec)
		}
	}
	return p
}

func webhookProvider(name string, cfg map[string]any, env func(string) string) webhook.Provider {
	return webhook.Provider{
		Name:         name,
		Path:         cfgStr(cfg, "inbound_path", env),
		Auth:         cfgStr(cfg, "auth", env),
		Secret:       cfgStr(cfg, "signature_secret", env),
		SigHeader:    cfgStr(cfg, "signature_header", env),
		APIKey:       cfgStr(cfg, "api_key", env),
		APIKeyHeader: cfgStr(cfg, "api_key_header", env),
		MaxBytes:     cfgInt(cfg, "max_payload_bytes"),
		CallbackURL:  cfgStr(cfg, "callback_url", env),
	}
}

func cronProvider(name string, cfg map[string]any, p *Plan) (cron.Provider, bool) {
	expr := cfgStr(cfg, "schedule", nil)
	sched, err := cron.Parse(expr)
	if err != nil {
		p.Warnings = append(p.Warnings, "provider "+name+": bad cron schedule "+err.Error())
		return cron.Provider{}, false
	}
	return cron.Provider{Name: name, Schedule: sched}, true
}

func rssProvider(appID, name string, cfg map[string]any, env func(string) string) rss.Provider {
	sec := cfgInt(cfg, "interval")
	if sec <= 0 {
		sec = 300
	}
	return rss.Provider{
		Name:      name,
		URL:       cfgStr(cfg, "url", env),
		CursorKey: processor.TriggerID(appID, name),
		Interval:  time.Duration(sec) * time.Second,
	}
}

func telegramProvider(appID, name string, cfg map[string]any, env func(string) string) telegram.Provider {
	sec := cfgInt(cfg, "interval")
	if sec <= 0 {
		sec = 1
	}
	return telegram.Provider{
		Name:      name,
		Token:     cfgStr(cfg, "token", env),
		CursorKey: processor.TriggerID(appID, name),
		Interval:  time.Duration(sec) * time.Second,
		APIBase:   cfgStr(cfg, "api_base", env),
	}
}

func discordProvider(name string, cfg map[string]any, env func(string) string) discord.Provider {
	return discord.Provider{
		Name:    name,
		Token:   cfgStr(cfg, "token", env),
		Intents: int(cfgInt(cfg, "intents")),
		APIBase: cfgStr(cfg, "api_base", env),
	}
}

func whatsappProvider(name string, cfg map[string]any, env func(string) string) whatsapp.Provider {
	return whatsapp.Provider{
		Name:          name,
		Path:          cfgStr(cfg, "inbound_path", env),
		AppSecret:     cfgStr(cfg, "app_secret", env),
		VerifyToken:   cfgStr(cfg, "verify_token", env),
		AccessToken:   cfgStr(cfg, "access_token", env),
		PhoneNumberID: cfgStr(cfg, "phone_number_id", env),
		APIBase:       cfgStr(cfg, "api_base", env),
		APIVersion:    cfgStr(cfg, "api_version", env),
	}
}

// storeCursors is the durable cursor store for pollers, backed by the trigger
// row's cursor column (the design's "cursors are columns, committed with the
// job" durability).
type storeCursors struct{ st *store.Store }

func (c storeCursors) Cursor(ctx context.Context, key string) string {
	tr, err := c.st.GetTrigger(ctx, key)
	if err != nil {
		return ""
	}
	return tr.Cursor
}

func (c storeCursors) SetCursor(ctx context.Context, key, value string) error {
	return c.st.SetCursor(ctx, key, value)
}

// Arm applies a plan: builds the webhook/cron adapters from the plan's providers,
// registers them, and arms every trigger. Returns the manager + registry, ready
// for the service to Start.
func Arm(ctx context.Context, st *store.Store, plan Plan, log *slog.Logger) (*processor.Manager, *adapter.Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	reg := adapter.NewRegistry()
	if len(plan.Webhooks) > 0 {
		reg.Register(webhook.New(plan.Webhooks))
	}
	if len(plan.Crons) > 0 {
		reg.Register(cron.New(plan.Crons))
	}
	if len(plan.Feeds) > 0 {
		reg.Register(rss.New(plan.Feeds, storeCursors{st: st}, log))
	}
	if len(plan.Telegrams) > 0 {
		reg.Register(telegram.New(plan.Telegrams, storeCursors{st: st}, log))
	}
	if len(plan.WhatsApps) > 0 {
		reg.Register(whatsapp.New(plan.WhatsApps, log))
	}
	if len(plan.Discords) > 0 {
		reg.Register(discord.New(plan.Discords, log))
	}
	mgr := processor.NewManager(st, reg)
	for _, t := range plan.Triggers {
		if _, err := mgr.Arm(ctx, t); err != nil {
			return nil, nil, err
		}
	}
	for _, w := range plan.Warnings {
		log.Warn("background: discovery", "warn", w)
	}
	log.Info("background: armed channels",
		"apps_triggers", len(plan.Triggers), "webhooks", len(plan.Webhooks),
		"crons", len(plan.Crons), "feeds", len(plan.Feeds),
		"telegram", len(plan.Telegrams), "whatsapp", len(plan.WhatsApps),
		"discord", len(plan.Discords))
	return mgr, reg, nil
}

// Rescan re-reads the apps dir on an interval and re-arms triggers (idempotent
// by stable trigger id), picking up config changes + new triggers on already-
// running adapters. New webhook paths / cron schedules need a restart (the
// adapters are built once at boot). Blocks until ctx is cancelled.
func Rescan(ctx context.Context, mgr *processor.Manager, dir string, every time.Duration, env func(string) string, log *slog.Logger) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			apps, err := ScanApps(dir)
			if err != nil {
				continue
			}
			plan := BuildPlan(apps, env)
			for _, tr := range plan.Triggers {
				_, _ = mgr.Arm(ctx, tr)
			}
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

var placeholderRe = regexp.MustCompile(`\{\{\s*(env|secret)\.([A-Za-z0-9_]+)\s*\}\}`)

// cfgStr reads a string config value and resolves {{env.X}} / {{secret.X}} from
// the env func ({{secret.X}} reads DIGITORN_BG_SECRET_X).
func cfgStr(cfg map[string]any, key string, env func(string) string) string {
	s, _ := cfg[key].(string)
	if s == "" || env == nil {
		return s
	}
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		g := placeholderRe.FindStringSubmatch(m)
		if g[1] == "secret" {
			return env("DIGITORN_BG_SECRET_" + g[2])
		}
		return env(g[2])
	})
}

func cfgInt(cfg map[string]any, key string) int64 {
	switch v := cfg[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
