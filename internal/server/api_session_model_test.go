package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// modelAppMgr is a fakeAppMgr that also serves a GetApp (for the BYOK flag).
type modelAppMgr struct {
	fakeAppMgr
	app *appmgr.App
}

func (m *modelAppMgr) GetApp(context.Context, string) (*appmgr.App, error) { return m.app, nil }

func twoAgentModelApp(appID string) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: appID, Name: appID, Version: "1.0"},
			Agents: []schema.Agent{
				{ID: "main", Role: "assistant", Brain: schema.Brain{
					Provider: "openai", Model: "gpt-4o-mini", Kind: "chat",
					Models: []string{"gpt-4o"},
				}},
				{ID: "explorer", Role: "explorer", Brain: schema.Brain{
					Provider: "anthropic", Model: "claude-haiku", Kind: "chat",
					Models: []string{"claude-sonnet"},
				}},
			},
			Runtime: &schema.RuntimeBlock{EntryAgent: "main"},
		},
	}
}

// newModelHarness wires the harness with a 2-agent app + a controllable BYOK flag.
func newModelHarness(t *testing.T, appID string, byok bool) *apiHarness {
	t.Helper()
	h := newAPIHarness(t)
	h.daemon.appMgr = &modelAppMgr{
		fakeAppMgr: fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{appID: twoAgentModelApp(appID)}},
		app:        &appmgr.App{AppID: appID, Enabled: true, BYOK: byok},
	}
	return h
}

// agentRow finds one agent entry in the GET /model response.
func agentRow(t *testing.T, body []byte, id string) map[string]any {
	t.Helper()
	var got map[string]any
	decodeBody(t, body, &got)
	rows, _ := got["agents"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["agent"] == id {
			return m
		}
	}
	t.Fatalf("agent %q not in response: %s", id, string(body))
	return nil
}

func createModelSession(t *testing.T, h *apiHarness, appID, user string) string {
	t.Helper()
	code, body := h.do(t, "POST", "/api/apps/"+appID+"/sessions", user, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %s", code, string(body))
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatal("missing session_id")
	}
	return sid
}

// Gateway mode (BYOK=false) : GET lists every agent's default/declared/kind, and a
// per-agent PUT pins a model on exactly that agent (the gateway is unreachable in
// the test, so validation is lenient — the persistence path is what we prove).
func TestSessionModel_PerAgent_GatewayMode(t *testing.T) {
	const app, user = "model-app", "user-A"
	h := newModelHarness(t, app, false)
	sid := createModelSession(t, h, app, user)
	base := "/api/apps/" + app + "/sessions/" + sid + "/model"

	// GET — no overrides yet.
	code, body := h.do(t, "GET", base, user, "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, string(body))
	}
	var top map[string]any
	decodeBody(t, body, &top)
	if top["entry"] != "main" || top["byok"] != false {
		t.Fatalf("top shape: %+v", top)
	}
	main := agentRow(t, body, "main")
	if main["entry"] != true || main["default"] != "gpt-4o-mini" || main["kind"] != "chat" {
		t.Fatalf("main row: %+v", main)
	}
	if main["override"] != "" || main["model"] != "gpt-4o-mini" {
		t.Fatalf("main should start un-overridden: %+v", main)
	}

	// PUT main → gpt-4o.
	code, body = h.do(t, "PUT", base, user, `{"agent":"main","model":"gpt-4o"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT main: %d %s", code, string(body))
	}
	// PUT explorer (sub-agent) → claude-sonnet.
	code, body = h.do(t, "PUT", base, user, `{"agent":"explorer","model":"claude-sonnet"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT explorer: %d %s", code, string(body))
	}

	_, body = h.do(t, "GET", base, user, "")
	main = agentRow(t, body, "main")
	exp := agentRow(t, body, "explorer")
	if main["override"] != "gpt-4o" || main["model"] != "gpt-4o" {
		t.Fatalf("main override not applied: %+v", main)
	}
	if exp["override"] != "claude-sonnet" || exp["model"] != "claude-sonnet" {
		t.Fatalf("explorer override not applied: %+v", exp)
	}

	// Clear main (empty model, no agent → defaults to the entry agent).
	code, body = h.do(t, "PUT", base, user, `{"model":""}`)
	if code != http.StatusOK {
		t.Fatalf("clear main: %d %s", code, string(body))
	}
	_, body = h.do(t, "GET", base, user, "")
	main = agentRow(t, body, "main")
	exp = agentRow(t, body, "explorer")
	if main["override"] != "" || main["model"] != "gpt-4o-mini" {
		t.Fatalf("main not reverted: %+v", main)
	}
	if exp["override"] != "claude-sonnet" {
		t.Fatalf("clearing main must not touch explorer: %+v", exp)
	}

	// Unknown agent → 400.
	code, _ = h.do(t, "PUT", base, user, `{"agent":"ghost","model":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown agent: want 400, got %d", code)
	}
}

// Direct/BYOK mode : a switch is allowed ONLY among the agent's declared models.
func TestSessionModel_PerAgent_DirectModeWhitelist(t *testing.T) {
	const app, user = "byok-app", "user-A"
	h := newModelHarness(t, app, true)
	sid := createModelSession(t, h, app, user)
	base := "/api/apps/" + app + "/sessions/" + sid + "/model"

	// A model the brain does not declare → rejected.
	code, body := h.do(t, "PUT", base, user, `{"agent":"main","model":"gpt-4o-turbo-unlisted"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("undeclared model: want 400, got %d %s", code, string(body))
	}
	// A declared alternative → accepted.
	code, body = h.do(t, "PUT", base, user, `{"agent":"main","model":"gpt-4o"}`)
	if code != http.StatusOK {
		t.Fatalf("declared model: %d %s", code, string(body))
	}
	_, body = h.do(t, "GET", base, user, "")
	if agentRow(t, body, "main")["override"] != "gpt-4o" {
		t.Fatalf("declared override not persisted: %s", string(body))
	}
}
