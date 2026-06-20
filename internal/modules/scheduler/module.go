// Package scheduler exposes one LLM-facing tool — schedule — that lets an agent
// program a recurring wake-up of ITS OWN session. The agent calls it ("remind me
// every morning at 9"), and the module hands the request to the background
// service's ops API (POST /ops/schedules): the service arms a cron bound to this
// session and, at each fire, re-runs the session with the given message injected
// (the agent keeps all of its accumulated context). The module holds no scheduler
// state itself — it is a thin, gated bridge from the agent to the durable bg
// service, so a daemon restart never loses a schedule.
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/flexjson"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// Config is the per-app configuration. BackgroundURL/OpsToken locate the bg
// service (daemon-global, so they default from env); DefaultReply sets how a
// wake replies on the channel when the agent doesn't specify.
type Config struct {
	BackgroundURL string `json:"background_url" yaml:"background_url"`
	OpsToken      string `json:"ops_token" yaml:"ops_token"`
	DefaultReply  string `json:"default_reply" yaml:"default_reply"`
}

// Module is the scheduler bridge.
type Module struct {
	module.Base

	mu     sync.RWMutex
	cfg    Config
	client *http.Client
}

// New constructs the scheduler module with its single tool wired.
func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          "scheduler",
		Version:     "1.0.0",
		Description: "Schedule a recurring wake-up of this session: at each cron time the session re-runs with an injected message.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux, domainmodule.PlatformMacOS, domainmodule.PlatformWindows,
		},
	}
	m.RegisterTool(module.Tool{
		Name: "schedule",
		Description: "Schedule a RECURRING wake-up of THIS session on a 5-field cron schedule. " +
			"At each fire, the session is re-run with `message` injected as a new user turn — the agent keeps all of " +
			"its accumulated context. Use for reminders, daily digests, periodic checks (e.g. '0 9 * * *' = 9am daily). " +
			"Returns the next run time. The schedule is durable (survives restarts).",
		Params: []tool.ParamSpec{
			{Name: "schedule", Type: "string", Description: "5-field cron expression: minute hour day-of-month month day-of-week. e.g. '0 9 * * *' for 9am every day, '*/15 * * * *' every 15 min.", Required: true},
			{Name: "message", Type: "string", Description: "The instruction to run at each fire, injected as the user message.", Required: true},
			{Name: "context", Type: "string", Description: "Optional extra context to inject alongside the message."},
			{Name: "reply", Type: "string", Description: "How the wake replies on the channel.", Enum: []any{"auto", "none", "stream"}},
			{Name: "reports", Type: "boolean", Description: "When true, each run gets a dated, downloadable folder (attachments/<timestamp>/) and the agent is told to write any file/report it produces there. Use for digests or anything the user should be able to download."},
		},
		RiskLevel: tool.RiskLow,
		Tags:      []string{"schedule", "cron", "reminder", "background"},
		Aliases:   []string{"schedule", "remind me", "planifier", "rappel", "programmer"},
		CLILabel:  "Schedule",
		CLIParam:  "schedule",
		Handler:   m.schedule,
	})
	return m
}

func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	var c Config
	if err := m.BindConfig(cfg, &c); err != nil {
		return err
	}
	m.apply(c)
	return nil
}

func (m *Module) UpdateConfig(ctx context.Context, cfg map[string]any) error {
	var c Config
	if err := m.BindConfig(cfg, &c); err != nil {
		return err
	}
	m.apply(c)
	return nil
}

func (m *Module) apply(c Config) {
	if c.BackgroundURL == "" {
		c.BackgroundURL = envOr("DIGITORN_BG_URL", "http://127.0.0.1:8090")
	}
	c.BackgroundURL = strings.TrimRight(c.BackgroundURL, "/")
	if c.OpsToken == "" {
		c.OpsToken = os.Getenv("DIGITORN_BG_OPS_TOKEN")
	}
	if c.DefaultReply == "" {
		c.DefaultReply = "auto"
	}
	m.mu.Lock()
	m.cfg = c
	m.client = &http.Client{Timeout: 10 * time.Second}
	m.mu.Unlock()
}

func (m *Module) snapshot() (Config, *http.Client) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg, m.client
}

// effectiveConfig layers the CALLING app's per-module config (delivered per
// invocation in ctx) over the Init-time defaults. This is the only correct path
// for a shared in-proc instance : the singleton's m.cfg is whatever the first
// app set, so the app-specific background_url / ops_token / default_reply must
// come from ctx, never from Init.
func (m *Module) effectiveConfig(ctx context.Context) (Config, *http.Client) {
	base, client := m.snapshot()
	if raw := module.ModuleConfigFrom(ctx); len(raw) > 0 {
		var c Config
		if err := m.BindConfig(raw, &c); err == nil {
			if c.BackgroundURL != "" {
				base.BackgroundURL = strings.TrimRight(c.BackgroundURL, "/")
			}
			if c.OpsToken != "" {
				base.OpsToken = c.OpsToken
			}
			if c.DefaultReply != "" {
				base.DefaultReply = c.DefaultReply
			}
		}
	}
	return base, client
}

// schedule programs the wake-up by handing it to the background service.
func (m *Module) schedule(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Schedule string `json:"schedule"`
		Message  string `json:"message"`
		Context  string `json:"context"`
		Reply    string `json:"reply"`
		Reports  flexjson.Bool   `json:"reports"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return tool.Result{Success: false, Error: "invalid params: " + err.Error()}, nil
	}
	if strings.TrimSpace(p.Schedule) == "" || strings.TrimSpace(p.Message) == "" {
		return tool.Result{Success: false, Error: "schedule and message are required"}, nil
	}
	id, _ := tool.IdentityFromContext(ctx)
	sess, app, owner := id.SessionID, id.AppID, id.UserID
	if sess == "" || app == "" {
		return tool.Result{Success: false, Error: "scheduler requires a session context"}, nil
	}

	cfg, client := m.effectiveConfig(ctx)
	reply := p.Reply
	if reply == "" {
		reply = cfg.DefaultReply
	}
	body, _ := json.Marshal(map[string]any{
		"app_id": app, "session_id": sess, "owner": owner,
		"schedule": p.Schedule, "message": p.Message, "context": p.Context, "reply": reply,
		"reports": p.Reports,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BackgroundURL+"/ops/schedules", bytes.NewReader(body))
	if err != nil {
		return tool.Result{Success: false, Error: err.Error()}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.OpsToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.OpsToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return tool.Result{Success: false, Error: "could not reach the background scheduler: " + err.Error()}, nil
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return tool.Result{Success: false, Error: fmt.Sprintf("scheduler refused (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(rb)))}, nil
	}
	var out map[string]any
	_ = json.Unmarshal(rb, &out)
	next, _ := out["next_run"].(string)

	confirm := "Recurring wake-up scheduled for this session."
	if next != "" {
		confirm = "Recurring wake-up scheduled. Next run: " + next
	}
	return tool.Result{
		Success:  true,
		Data:     confirm,
		Metadata: map[string]any{"schedule": p.Schedule, "next_run": next, "id": out["id"]},
		Display:  &tool.DisplayHint{Type: "text", Title: "Schedule", Summary: p.Schedule},
	}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
