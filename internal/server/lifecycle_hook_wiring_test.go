package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// recordingLifecycle captures FireLifecycle calls so a test can assert
// the HTTP layer fired the right lifecycle hook event.
type recordingLifecycle struct {
	mu    sync.Mutex
	calls []recordedLifecycleFire
}

type recordedLifecycleFire struct {
	event           schema.HookEvent
	appID, sid, uid string
}

func (r *recordingLifecycle) FireLifecycle(_ context.Context, event schema.HookEvent, appID, sessionID, userID string) hooks.FireResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedLifecycleFire{event: event, appID: appID, sid: sessionID, uid: userID})
	return hooks.FireResult{}
}

func (r *recordingLifecycle) only(event schema.HookEvent) *recordedLifecycleFire {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.calls {
		if r.calls[i].event == event {
			c := r.calls[i]
			return &c
		}
	}
	return nil
}

// TestServer_DeleteSession_FiresSessionEnd proves the HTTP session-delete
// handler fires the session_end lifecycle hook (through the injectable
// lifecycleFirer the daemon wires to *runtime.Engine) with the session's
// own app/user identity, BEFORE the session is torn down.
func TestServer_DeleteSession_FiresSessionEnd(t *testing.T) {
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 4096,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond, Fsync: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := sessionstore.NewBus(sessionstore.BusConfig{
		Paths: paths, Flusher: flusher,
		EvictionInterval: time.Hour, StateIdleEvictAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})

	fake := &recordingLifecycle{}
	d := &Daemon{
		sessionStore:    bus,
		sessionFlusher:  flusher,
		sessionPaths:    paths,
		envelopeBuilder: sessionstore.NewEnvelopeBuilder("inst-lc", []string{"chat"}),
		lifecycle:       fake,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware)
		r.Post("/api/apps/{app_id}/sessions", d.createSession)
		r.Delete("/api/apps/{app_id}/sessions/{session_id}", d.deleteSession)
	})

	do := func(method, path, user, body string) (int, []byte) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		if user != "" {
			req.Header.Set("X-User-ID", user)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	// Create a session owned by user "u1" under app "lc-app".
	code, body := do("POST", "/api/apps/lc-app/sessions", "u1", `{"title":"x"}`)
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %s", code, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session_id: %s", body)
	}

	// Delete it — must fire session_end before teardown.
	code, body = do("DELETE", "/api/apps/lc-app/sessions/"+sid, "u1", "")
	if code != http.StatusOK {
		t.Fatalf("delete session: %d %s", code, body)
	}

	got := fake.only(schema.HookEventSessionEnd)
	if got == nil {
		t.Fatal("deleteSession did NOT fire session_end")
	}
	if got.appID != "lc-app" || got.sid != sid || got.uid != "u1" {
		t.Errorf("session_end fired with wrong identity: %+v (want app=lc-app sid=%s uid=u1)", got, sid)
	}
}
