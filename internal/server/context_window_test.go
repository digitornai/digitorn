package server

import (
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/contextsvc"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

func windowApp(appID string) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: appID, Name: appID, Version: "1.0"},
			Agents: []schema.Agent{
				{ID: "main", Role: "assistant", Brain: schema.Brain{
					Provider: "openai", Model: "deepseek-v4-flash", Kind: "chat",
					Context: &schema.ContextConfig{MaxTokens: 1000000},
				}},
			},
			Runtime: &schema.RuntimeBlock{EntryAgent: "main"},
		},
	}
}

func setGatewayWindows(t *testing.T, m map[string]int) {
	t.Helper()
	gwCatalog.mu.Lock()
	gwCatalog.windows, gwCatalog.at = m, time.Now()
	gwCatalog.mu.Unlock()
	t.Cleanup(func() {
		gwCatalog.mu.Lock()
		gwCatalog.windows, gwCatalog.kinds, gwCatalog.at = nil, nil, time.Time{}
		gwCatalog.mu.Unlock()
	})
}

// The context gauge denominator must track the per-session SELECTED model and the
// gateway's documented window — not the YAML-static brain default.
func TestSessionWindowBrain_FollowsSelectedModelAndGatewayWindow(t *testing.T) {
	const app = "win-app"
	d := &Daemon{appMgr: &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{app: windowApp(app)}}}
	snap := sessionstore.SessionSnapshot{
		AppID:          app,
		EntryAgent:     "main",
		ModelOverrides: map[string]string{"main": "mimo-v2.5"},
	}

	// Gateway window unknown (catalog not warm yet) : the selected model is still
	// applied, and the author's explicit context.max_tokens is kept. We never fall
	// back to a hardcoded per-model table — the real window comes from the gateway
	// catalog once warm.
	setGatewayWindows(t, nil)
	b := d.sessionWindowBrain(snap)
	if b.Model != "mimo-v2.5" {
		t.Fatalf("selected model not applied: got %q", b.Model)
	}
	if b.Context == nil || b.Context.MaxTokens != 1000000 {
		t.Fatalf("expected configured window kept for unknown override model, got %+v", b.Context)
	}

	// Gateway reports the real window for the selected model : it wins, and feeds
	// Resolve as the gauge denominator.
	setGatewayWindows(t, map[string]int{"mimo-v2.5": 262144})
	b = d.sessionWindowBrain(snap)
	if b.Context == nil || b.Context.MaxTokens != 262144 {
		t.Fatalf("gateway window did not win: %+v", b.Context)
	}
	if got := contextsvc.Resolve(snap, b).Window; got != 262144 {
		t.Fatalf("Resolve window = %d, want 262144", got)
	}
}

// Without an override the entry agent's own model is used, and an unknown gateway
// window leaves the configured budget intact (no regression for static apps).
func TestSessionWindowBrain_NoOverrideKeepsConfiguredWindow(t *testing.T) {
	const app = "win-app2"
	d := &Daemon{appMgr: &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{app: windowApp(app)}}}
	snap := sessionstore.SessionSnapshot{AppID: app, EntryAgent: "main"}
	setGatewayWindows(t, nil)
	b := d.sessionWindowBrain(snap)
	if b.Model != "deepseek-v4-flash" {
		t.Fatalf("entry model wrong: %q", b.Model)
	}
	if b.Context == nil || b.Context.MaxTokens != 1000000 {
		t.Fatalf("configured window wrong: %+v", b.Context)
	}
}
