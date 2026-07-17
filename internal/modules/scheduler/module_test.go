package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

func ctxWith(sess, app, user string) context.Context {
	return tool.WithIdentity(context.Background(), tool.Identity{SessionID: sess, AppID: app, UserID: user})
}

func TestSchedule_PostsToBackground(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "trg-abc", "next_run": "2026-06-12T09:00:00Z", "armed": true})
	}))
	defer srv.Close()

	m := New()
	if err := m.Init(context.Background(), map[string]any{"background_url": srv.URL, "ops_token": "tok"}); err != nil {
		t.Fatalf("init: %v", err)
	}

	res, err := m.schedule(ctxWith("sess-1", "myapp", "u1"),
		json.RawMessage(`{"schedule":"0 9 * * *","message":"daily digest","reply":"none"}`))
	if err != nil || !res.Success {
		t.Fatalf("want success, got %+v err=%v", res, err)
	}
	if gotPath != "/ops/schedules" {
		t.Errorf("wrong path: %q", gotPath)
	}
	if gotBody["app_id"] != "myapp" || gotBody["session_id"] != "sess-1" || gotBody["owner"] != "u1" {
		t.Fatalf("ctx identity not forwarded: %+v", gotBody)
	}
	if gotBody["schedule"] != "0 9 * * *" || gotBody["message"] != "daily digest" || gotBody["reply"] != "none" {
		t.Fatalf("schedule body wrong: %+v", gotBody)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("bearer token not forwarded: %q", gotAuth)
	}
	if res.Metadata["next_run"] != "2026-06-12T09:00:00Z" {
		t.Errorf("next_run not surfaced to the model: %+v", res.Metadata)
	}
}

func TestSchedule_PerAppConfigFromCtx(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	defer srv.Close()

	m := New()
	_ = m.Init(context.Background(), map[string]any{"background_url": "http://127.0.0.1:1"})
	ctx := module.WithModuleConfig(ctxWith("s", "a", "u"), map[string]any{"background_url": srv.URL})

	res, err := m.schedule(ctx, json.RawMessage(`{"schedule":"* * * * *","message":"x"}`))
	if err != nil || !res.Success {
		t.Fatalf("want success via ctx config, got %+v err=%v", res, err)
	}
	if !hit {
		t.Fatal("ctx per-app background_url was ignored (hit the Init default instead)")
	}
}

func TestSchedule_DefaultReply(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x"})
	}))
	defer srv.Close()
	m := New()
	_ = m.Init(context.Background(), map[string]any{"background_url": srv.URL, "default_reply": "stream"})

	if _, err := m.schedule(ctxWith("s", "a", "u"), json.RawMessage(`{"schedule":"* * * * *","message":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if gotBody["reply"] != "stream" {
		t.Fatalf("default reply not applied: %+v", gotBody)
	}
}

func TestSchedule_Validations(t *testing.T) {
	m := New()
	_ = m.Init(context.Background(), map[string]any{"background_url": "http://127.0.0.1:0"})

	if res, _ := m.schedule(ctxWith("s", "a", "u"), json.RawMessage(`{"message":"x"}`)); res.Success {
		t.Error("missing schedule must fail")
	}
	if res, _ := m.schedule(ctxWith("s", "a", "u"), json.RawMessage(`{"schedule":"* * * * *"}`)); res.Success {
		t.Error("missing message must fail")
	}
	if res, _ := m.schedule(context.Background(), json.RawMessage(`{"schedule":"* * * * *","message":"x"}`)); res.Success {
		t.Error("missing session context must fail")
	}
}

func TestSchedule_BackgroundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"error":"invalid cron schedule"}`)
	}))
	defer srv.Close()
	m := New()
	_ = m.Init(context.Background(), map[string]any{"background_url": srv.URL})

	res, _ := m.schedule(ctxWith("s", "a", "u"), json.RawMessage(`{"schedule":"nope","message":"x"}`))
	if res.Success || !strings.Contains(res.Error, "422") {
		t.Fatalf("a bg refusal must surface as an error, got %+v", res)
	}
}
