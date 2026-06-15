package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/config"
)

// fakeOps is a minimal bg /ops server : two users' schedules, a run for each,
// and it records what the daemon relays (the created body, the toggled id).
func fakeOps(t *testing.T) (*httptest.Server, *map[string]any, *[]string) {
	t.Helper()
	var created map[string]any
	var toggled []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ops/schedules" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"schedules": []map[string]any{
				{"id": "trg-alice", "app_id": "app1", "session_id": "s-alice", "owner": "alice", "schedule": "* * * * *", "enabled": true},
				{"id": "trg-bob", "app_id": "app1", "session_id": "s-bob", "owner": "bob", "schedule": "0 9 * * *", "enabled": true},
			}})
		case r.URL.Path == "/ops/schedules" && r.Method == http.MethodPost:
			b, _ := json.NewDecoder(r.Body), 0
			_ = b
			created = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "trg-new", "armed": true})
		case strings.HasPrefix(r.URL.Path, "/ops/triggers/") && r.Method == http.MethodPost:
			toggled = append(toggled, strings.TrimPrefix(r.URL.Path, "/ops/triggers/"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.URL.Path == "/ops/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []map[string]any{
				{"id": "r1", "trigger_id": "trg-alice", "app_id": "app1", "outcome": "ok"},
				{"id": "r2", "trigger_id": "trg-bob", "app_id": "app1", "outcome": "ok"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, &created, &toggled
}

func automationsDaemon(opsURL string) *Daemon {
	return &Daemon{cfg: &config.Config{Background: config.Background{OpsURL: opsURL, OpsToken: "t"}}}
}

// asUser injects the authenticated user into the request context (the JWT
// middleware's job in production).
func asUser(r *http.Request, user string) *http.Request {
	return r.WithContext(withUserID(r.Context(), user))
}

// A user lists ONLY their schedules — never another user's.
func TestAutomations_ListIsUserScoped(t *testing.T) {
	srv, _, _ := fakeOps(t)
	defer srv.Close()
	d := automationsDaemon(srv.URL)

	rec := httptest.NewRecorder()
	d.listAutomationSchedules(rec, asUser(httptest.NewRequest("GET", "/api/automations/schedules", nil), "alice"))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "trg-alice") || strings.Contains(body, "trg-bob") {
		t.Fatalf("cross-user leak in list: %s", body)
	}
}

// Create FORCES owner = the authenticated caller, even when the body lies.
func TestAutomations_CreateForcesOwner(t *testing.T) {
	srv, created, _ := fakeOps(t)
	defer srv.Close()
	d := automationsDaemon(srv.URL)

	payload := `{"app_id":"app1","session_id":"s-x","schedule":"* * * * *","message":"go","owner":"bob"}`
	req := asUser(httptest.NewRequest("POST", "/api/automations/schedules", strings.NewReader(payload)), "alice")
	rec := httptest.NewRecorder()
	d.createAutomationSchedule(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if (*created)["owner"] != "alice" {
		t.Fatalf("owner must be the caller (alice), got %v — body-supplied owner must be ignored", (*created)["owner"])
	}
}

// Toggling another user's schedule is a 404 (existence not leaked) and nothing
// is relayed to the bg.
func TestAutomations_ToggleOwnershipEnforced(t *testing.T) {
	srv, _, toggled := fakeOps(t)
	defer srv.Close()
	d := automationsDaemon(srv.URL)

	call := func(user, id string) int {
		r := chi.NewRouter()
		r.Post("/api/automations/schedules/{id}/disable", d.toggleAutomationSchedule(false))
		req := asUser(httptest.NewRequest("POST", "/api/automations/schedules/"+id+"/disable", nil), user)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("alice", "trg-bob"); code != 404 {
		t.Fatalf("alice disabling bob's schedule must 404, got %d", code)
	}
	if len(*toggled) != 0 {
		t.Fatalf("cross-user toggle must never reach the bg, relayed: %v", *toggled)
	}
	if code := call("alice", "trg-alice"); code != 200 {
		t.Fatalf("alice disabling her own schedule must 200, got %d", code)
	}
	if len(*toggled) != 1 || !strings.HasPrefix((*toggled)[0], "trg-alice/") {
		t.Fatalf("own toggle must relay exactly once: %v", *toggled)
	}
}

// Runs are filtered to those born from the caller's triggers.
func TestAutomations_RunsAreUserScoped(t *testing.T) {
	srv, _, _ := fakeOps(t)
	defer srv.Close()
	d := automationsDaemon(srv.URL)

	rec := httptest.NewRecorder()
	d.listAutomationRuns(rec, asUser(httptest.NewRequest("GET", "/api/automations/runs", nil), "bob"))
	body := rec.Body.String()
	if !strings.Contains(body, `"r2"`) || strings.Contains(body, `"r1"`) {
		t.Fatalf("runs cross-user leak: %s", body)
	}
}

// No ops URL configured → graceful 404, never a panic.
func TestAutomations_UnconfiguredIsGraceful(t *testing.T) {
	d := automationsDaemon("")
	rec := httptest.NewRecorder()
	d.listAutomationSchedules(rec, asUser(httptest.NewRequest("GET", "/api/automations/schedules", nil), "alice"))
	if rec.Code != 404 {
		t.Fatalf("unconfigured must 404, got %d", rec.Code)
	}
}

// Pure-function locks.
func TestOwnSchedulesAndRuns(t *testing.T) {
	all := []opsSchedule{{ID: "a", Owner: "u1"}, {ID: "b", Owner: "u2"}, {ID: "c", Owner: ""}}
	mine := ownSchedules(all, "u1")
	if len(mine) != 1 || mine[0].ID != "a" {
		t.Fatalf("ownSchedules wrong: %+v", mine)
	}
	// Ownerless rows never match anyone (incl. the empty user).
	if got := ownSchedules(all, ""); len(got) != 0 {
		t.Fatalf("empty user must own nothing, got %+v", got)
	}
	runs := ownRuns([]opsRun{{ID: "r1", TriggerID: "a"}, {ID: "r2", TriggerID: "b"}}, map[string]bool{"a": true})
	if len(runs) != 1 || runs[0].ID != "r1" {
		t.Fatalf("ownRuns wrong: %+v", runs)
	}
	_ = context.Background()
}
