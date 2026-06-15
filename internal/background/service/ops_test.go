package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/mbathepaul/digitorn/internal/background/store"
)

func opsStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "ops.db") + "?_pragma=busy_timeout(5000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	st := store.New(db)
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// opsServer mounts the ops API exactly as the service does (StripPrefix /ops).
func opsServer(t *testing.T, st *store.Store, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/ops", opsRoutes(st, OpsConfig{Token: token})))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, req)
}

func post(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestOps_ListAndGetTriggers(t *testing.T) {
	st := opsStore(t)
	ctx := context.Background()
	_ = st.UpsertTrigger(ctx, &store.Trigger{ID: "t1", AppID: "app", Provider: "tg", Adapter: "telegram", Enabled: true})
	_ = st.UpsertTrigger(ctx, &store.Trigger{ID: "t2", AppID: "app", Provider: "c", Adapter: "cron", Enabled: false,
		ConfigJSON: `{"schedule":"* * * * *"}`})
	_ = st.RecordRun(ctx, store.Run{ID: "r1", JobID: "j1", AppID: "app", TriggerID: "t1", Outcome: "ok", StartedAt: time.Now().UTC()})
	srv := opsServer(t, st, "")

	// List all.
	code, body := get(t, srv.URL+"/ops/triggers", "")
	if code != 200 || body["count"].(float64) != 2 {
		t.Fatalf("list: code=%d body=%v", code, body)
	}
	// enabled_only filter.
	code, body = get(t, srv.URL+"/ops/triggers?enabled_only=true", "")
	if body["count"].(float64) != 1 {
		t.Fatalf("enabled_only: %v", body)
	}
	// Detail of t1 carries stats + recent_runs.
	code, body = get(t, srv.URL+"/ops/triggers/t1", "")
	if code != 200 {
		t.Fatalf("detail: %d", code)
	}
	if _, ok := body["stats"]; !ok {
		t.Errorf("detail missing stats: %v", body)
	}
	if rr, ok := body["recent_runs"].([]any); !ok || len(rr) != 1 {
		t.Errorf("detail missing recent_runs: %v", body["recent_runs"])
	}
	// Cron trigger detail exposes next_run.
	code, body = get(t, srv.URL+"/ops/triggers/t2", "")
	if _, ok := body["next_run"]; !ok {
		t.Errorf("cron trigger should expose next_run: %v", body)
	}
	// Unknown trigger → 404.
	if code, _ = get(t, srv.URL+"/ops/triggers/ghost", ""); code != 404 {
		t.Errorf("unknown trigger want 404, got %d", code)
	}
}

func TestOps_EnableDisableTrigger(t *testing.T) {
	st := opsStore(t)
	_ = st.UpsertTrigger(context.Background(), &store.Trigger{ID: "t1", AppID: "app", Provider: "p", Adapter: "cron", Enabled: true})
	srv := opsServer(t, st, "")

	if code, _ := post(t, srv.URL+"/ops/triggers/t1/disable", ""); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	got, _ := st.GetTrigger(context.Background(), "t1")
	if got.Enabled {
		t.Fatal("trigger should be disabled")
	}
	if code, _ := post(t, srv.URL+"/ops/triggers/ghost/enable", ""); code != 404 {
		t.Errorf("enable unknown want 404, got %d", code)
	}
}

func TestOps_JobsAndRuns(t *testing.T) {
	st := opsStore(t)
	ctx := context.Background()
	j, _, _ := st.Enqueue(ctx, store.NewJob{AppID: "app", TriggerID: "t1", DedupKey: "d1", Payload: []byte(`{}`)})
	_ = st.RecordRun(ctx, store.Run{ID: "r1", JobID: j.ID, AppID: "app", TriggerID: "t1", Outcome: "ok", StartedAt: time.Now().UTC()})
	srv := opsServer(t, st, "")

	// List jobs.
	code, body := get(t, srv.URL+"/ops/jobs?app=app", "")
	if code != 200 || body["count"].(float64) != 1 {
		t.Fatalf("list jobs: %v", body)
	}
	// Job detail carries its runs.
	code, body = get(t, srv.URL+"/ops/jobs/"+j.ID, "")
	if code != 200 {
		t.Fatalf("job detail: %d", code)
	}
	if runs, ok := body["runs"].([]any); !ok || len(runs) != 1 {
		t.Errorf("job detail missing runs: %v", body["runs"])
	}
	// Runs list filtered by outcome.
	code, body = get(t, srv.URL+"/ops/runs?outcome=ok", "")
	if body["count"].(float64) != 1 {
		t.Fatalf("runs filter: %v", body)
	}
	if code, _ = get(t, srv.URL+"/ops/jobs/ghost", ""); code != 404 {
		t.Errorf("unknown job want 404, got %d", code)
	}
}

func TestOps_ReplayJob(t *testing.T) {
	st := opsStore(t)
	ctx := context.Background()
	j, _, _ := st.Enqueue(ctx, store.NewJob{AppID: "app", DedupKey: "d1", Payload: []byte(`{}`)})
	_, _ = st.Claim(ctx, 1, time.Minute)
	_ = st.Fail(ctx, j.ID, "boom", 0) // terminal
	srv := opsServer(t, st, "")

	if code, _ := post(t, srv.URL+"/ops/jobs/"+j.ID+"/replay", ""); code != 200 {
		t.Fatalf("replay: %d", code)
	}
	got, _ := st.Get(ctx, j.ID)
	if got.State != store.JobPending {
		t.Fatalf("replayed job should be pending, got %s", got.State)
	}
	// Replaying a now-pending job → 409 (not replayable).
	if code, _ := post(t, srv.URL+"/ops/jobs/"+j.ID+"/replay", ""); code != 409 {
		t.Errorf("replay pending want 409, got %d", code)
	}
}

func postBody(t *testing.T, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, req)
}

func TestOps_CreateTriggerRuntime(t *testing.T) {
	st := opsStore(t)
	var got CreateTriggerRequest
	rearm := func(ctx context.Context, req CreateTriggerRequest) (store.Trigger, error) {
		got = req
		tr := store.Trigger{ID: "rt1", AppID: req.AppID, Provider: req.Provider, Adapter: "cron",
			Enabled: true, ConfigJSON: `{"schedule":"` + req.Schedule + `"}`}
		_ = st.UpsertTrigger(ctx, &tr)
		return tr, nil
	}
	srv := httptest.NewServer(http.StripPrefix("/ops", opsRoutes(st, OpsConfig{Rearm: rearm})))
	t.Cleanup(srv.Close)

	code, resp := postBody(t, srv.URL+"/ops/triggers", "",
		`{"app_id":"a","provider":"nightly","adapter":"cron","schedule":"0 9 * * *","message":"daily report"}`)
	if code != 201 {
		t.Fatalf("create trigger: code=%d body=%v", code, resp)
	}
	if got.Provider != "nightly" || got.Schedule != "0 9 * * *" || got.Message != "daily report" {
		t.Fatalf("Rearm received wrong request: %+v", got)
	}
	if resp["armed"] != true {
		t.Errorf("response should report armed: %v", resp)
	}
	if _, ok := resp["next_run"]; !ok {
		t.Errorf("a cron trigger response should carry next_run: %v", resp)
	}

	// Missing required fields → 400.
	if code, _ := postBody(t, srv.URL+"/ops/triggers", "", `{"adapter":"cron"}`); code != 400 {
		t.Errorf("missing fields want 400, got %d", code)
	}
}

func TestOps_CreateTriggerNotImplemented(t *testing.T) {
	st := opsStore(t)
	srv := opsServer(t, st, "") // no Rearm hook wired
	if code, _ := postBody(t, srv.URL+"/ops/triggers", "",
		`{"app_id":"a","provider":"p","adapter":"cron","schedule":"* * * * *","message":"x"}`); code != 501 {
		t.Fatalf("without a Rearm hook, POST /ops/triggers should be 501, got %d", code)
	}
}

func TestOps_CreateAndListSchedule(t *testing.T) {
	st := opsStore(t)
	var got CreateTriggerRequest
	rearm := func(ctx context.Context, req CreateTriggerRequest) (store.Trigger, error) {
		got = req
		// Mirror the cmd closure: a TriggerSpec-shaped config binding the session.
		cfg := `{"app_id":"` + req.AppID + `","provider":"` + req.Provider +
			`","adapter":"cron","schedule":"` + req.Schedule +
			`","activation":{"Session":"` + req.Session + `","Owner":"` + req.Owner +
			`","Message":"` + req.Message + `","Reply":"` + req.Reply + `"}}`
		tr := store.Trigger{ID: "sch1", AppID: req.AppID, Provider: req.Provider, Adapter: "cron",
			Enabled: true, Kind: req.Kind, ConfigJSON: cfg}
		_ = st.UpsertTrigger(ctx, &tr)
		return tr, nil
	}
	srv := httptest.NewServer(http.StripPrefix("/ops", opsRoutes(st, OpsConfig{Rearm: rearm})))
	t.Cleanup(srv.Close)

	code, resp := postBody(t, srv.URL+"/ops/schedules", "",
		`{"app_id":"a","session_id":"sess-42","owner":"u1","schedule":"0 9 * * *","message":"daily digest","reply":"auto"}`)
	if code != 201 {
		t.Fatalf("create schedule: code=%d body=%v", code, resp)
	}
	if got.Kind != "schedule" || got.Session != "sess-42" || got.Owner != "u1" {
		t.Fatalf("Rearm got wrong schedule request: %+v", got)
	}
	if resp["session_id"] != "sess-42" || resp["schedule"] != "0 9 * * *" {
		t.Errorf("schedule view missing binding: %v", resp)
	}
	if _, ok := resp["next_run"]; !ok {
		t.Errorf("schedule should expose next_run: %v", resp)
	}

	// It lists on the schedules surface (not mixed with channel triggers).
	_, list := get(t, srv.URL+"/ops/schedules?app=a", "")
	if list["count"].(float64) != 1 {
		t.Fatalf("schedule list: %v", list)
	}

	// Missing session_id → 400.
	if code, _ := postBody(t, srv.URL+"/ops/schedules", "",
		`{"app_id":"a","schedule":"* * * * *","message":"x"}`); code != 400 {
		t.Errorf("missing session_id want 400, got %d", code)
	}
}

func TestOps_AuthToken(t *testing.T) {
	st := opsStore(t)
	_ = st.UpsertTrigger(context.Background(), &store.Trigger{ID: "t1", AppID: "app", Provider: "p", Adapter: "cron", Enabled: true})
	srv := opsServer(t, st, "s3cret")

	// No token → 401.
	if code, _ := get(t, srv.URL+"/ops/triggers", ""); code != 401 {
		t.Errorf("no token want 401, got %d", code)
	}
	// Wrong token → 401.
	if code, _ := get(t, srv.URL+"/ops/triggers", "wrong"); code != 401 {
		t.Errorf("wrong token want 401, got %d", code)
	}
	// Correct token → 200.
	if code, _ := get(t, srv.URL+"/ops/triggers", "s3cret"); code != 200 {
		t.Errorf("correct token want 200, got %d", code)
	}
}
