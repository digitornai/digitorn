package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/config"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// fakeRunner captures one engine.Run invocation.
type fakeRunner struct {
	mu      sync.Mutex
	got     runtime.TurnInput
	called  int
	respond *runtime.TurnResult
	err     error
	done    chan struct{}
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		done:    make(chan struct{}, 1),
		respond: &runtime.TurnResult{Seq: 99, Content: "fake"},
	}
}

func (f *fakeRunner) Run(_ context.Context, in runtime.TurnInput) (*runtime.TurnResult, error) {
	f.mu.Lock()
	f.got = in
	f.called++
	f.mu.Unlock()
	select {
	case f.done <- struct{}{}:
	default:
	}
	return f.respond, f.err
}

func (f *fakeRunner) waitCalled(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(d):
		t.Fatalf("fakeRunner.Run not called within %s", d)
	}
}

func (f *fakeRunner) snapshot() (runtime.TurnInput, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got, f.called
}

// minimalDaemon builds a Daemon-like struct stripped of HTTP/DB bits, just
// enough to test the API handlers via chi. It uses a real sessionstore +
// fake realtime so we can exercise the routes end-to-end.
type apiHarness struct {
	mux             *chi.Mux
	bus             *sessionstore.Bus
	flusher         *sessionstore.DiskFlusher
	paths           sessionstore.Paths
	bridge          *SocketIOBridge
	rt              *fakeRealtime
	envelopeBuilder *sessionstore.EnvelopeBuilder
	daemon          *Daemon
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths:            paths,
		NumShards:        2,
		QueueCapPerShard: 4096,
		BatchMax:         100,
		FlushInterval:    2 * time.Millisecond,
		Fsync:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := sessionstore.NewBus(sessionstore.BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		EvictionInterval:    1 * time.Hour,
		StateIdleEvictAfter: 1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRealtime()
	builder := sessionstore.NewEnvelopeBuilder("inst-api", []string{"chat", "tools"})
	bridge := NewSocketIOBridge(rt, bus, builder, paths, NullAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bridge.Start(context.Background())

	// Production always builds the Daemon with a non-nil *config.Config
	// (Build(cfg *config.Config) is the only constructor). Mirror that invariant
	// so handlers that read d.cfg (repushChannelTriggers, putSessionModel, …)
	// exercise the real prod path instead of panicking on a nil deref.
	cfg := config.Defaults()
	d := &Daemon{
		cfg:             &cfg,
		sessionStore:    bus,
		sessionFlusher:  flusher,
		sessionPaths:    paths,
		envelopeBuilder: builder,
		bridge:          bridge,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Route turns through the session runner, exactly like production. The
	// exec closure reads d.engine at call time so tests that set
	// h.daemon.engine after construction still drive turns through it.
	d.sessionRunner = newSessionRunner(func(ctx context.Context, in runtime.TurnInput) error {
		if d.engine == nil {
			return nil
		}
		_, err := d.engine.Run(ctx, in)
		return err
	}, turnSafetyCutoff, d.logger)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware)

		r.Route("/api/apps/{app_id}/sessions", func(r chi.Router) {
			r.Get("/", d.listSessions)
			r.Post("/", d.createSession)
			r.Get("/search", d.searchSessions)
			r.Route("/{session_id}", func(r chi.Router) {
				r.Get("/", d.getSession)
				r.Delete("/", d.deleteSession)
				r.Get("/model", d.getSessionModel)
				r.Put("/model", d.putSessionModel)
				r.Get("/history", d.getHistory)
				r.Get("/events", d.getEvents)
				r.Get("/state", d.getState)
				r.Get("/memory", d.getMemory)
				r.Get("/agents", d.getAgents)
				r.Get("/queue", d.getQueue)
				r.Post("/messages", d.postMessage)
				r.Post("/abort", d.abortTurn)
				r.Post("/compact", d.compactSession)
				r.Get("/tasks", d.listBackgroundTasks)
				r.Post("/tasks/{task_id}/cancel", d.cancelBackgroundTask)
				r.Post("/fork", d.forkSession)
				r.Get("/export", d.exportSession)
				r.Post("/resume", func(w http.ResponseWriter, r *http.Request) { notImplemented(w, "resume") })
			})
		})
		r.Get("/api/apps/{app_id}/approvals", d.listApprovals)
		r.Post("/api/apps/{app_id}/approve", d.resolveApproval)
		r.Get("/api/apps/{app_id}/required-secrets", d.requiredSecrets)
		r.Get("/api/apps/{app_id}/secrets", d.listSecrets)
		r.Get("/api/apps/{app_id}/secrets/{key}", d.getSecret)
		r.Put("/api/apps/{app_id}/secrets", d.setSecrets)
		r.Put("/api/apps/{app_id}/secrets/{key}", d.setSecret)
		r.Delete("/api/apps/{app_id}/secrets/{key}", d.deleteSecret)
		r.Get("/api/apps/{app_id}/diagnostics", d.diagnostics)
		r.Get("/api/apps/{app_id}/status", d.appStatus)
		r.Get("/api/apps/{app_id}/errors", d.appErrors)
		r.Get("/api/apps/{app_id}/ui-config", d.uiConfig)
		r.Get("/api/daemon/stats", d.daemonStats)
	})

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bridge.Stop(ctx)
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})

	return &apiHarness{
		mux: r, bus: bus, flusher: flusher, paths: paths,
		bridge: bridge, rt: rt, envelopeBuilder: builder, daemon: d,
	}
}

func (h *apiHarness) do(t *testing.T, method, path, user, body string) (int, []byte) {
	t.Helper()
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
	h.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func decodeBody(t *testing.T, b []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, string(b))
	}
}

// ---------- Sessions ----------

func TestAPI_CreateSessionThenGetSession(t *testing.T) {
	h := newAPIHarness(t)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A",
		`{"title":"Demo","workspace":"/ws"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, string(body))
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatal("missing session_id in response")
	}

	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid, "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, string(body))
	}
	var got map[string]any
	decodeBody(t, body, &got)
	if got["title"] != "Demo" || got["workspace"] != "/ws" {
		t.Fatalf("session shape: %+v", got)
	}
}

func TestAPI_GetSession_Forbidden_DifferentUser(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"t"}`)
	if code != http.StatusCreated {
		t.Fatal(string(body))
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, _ = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid, "user-B", "")
	if code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", code)
	}
}

func TestAPI_PostMessageThenHistory(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	if code != http.StatusCreated {
		t.Fatal(string(body))
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	for i := 0; i < 5; i++ {
		code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
			fmt.Sprintf(`{"content":"hello %d"}`, i))
		if code != http.StatusCreated {
			t.Fatalf("post msg %d: %d %s", i, code, string(body))
		}
	}

	// Flush so the JSONL is on disk for /history to read.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)

	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/history", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, string(body))
	}
	var hist map[string]any
	decodeBody(t, body, &hist)
	if hist["messages"] == nil || hist["events"] == nil || hist["pending_queue"] == nil {
		t.Fatalf("history shape missing required keys: %s", string(body))
	}
	msgs := hist["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages count: %d (want 5)", len(msgs))
	}
}

func TestAPI_AbortTurn_MarksInterrupted(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/abort", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("abort: %d %s", code, string(body))
	}

	state, _ := h.bus.State(sid)
	state.RLock()
	interrupted := state.Interrupted
	state.RUnlock()
	if !interrupted {
		t.Fatal("session not interrupted")
	}
}

func TestAPI_Compact_ReducesEventsAndReturnsCutoff(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	for i := 0; i < 20; i++ {
		h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
			`{"content":"x"}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/compact", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("compact: %d %s", code, string(body))
	}
	var res map[string]any
	decodeBody(t, body, &res)
	if res["cutoff_seq"] == nil || res["snapshot_sha256"] == nil {
		t.Fatalf("compact response missing keys: %s", string(body))
	}
}

func TestAPI_DeleteSession_RemovesDirectory(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, _ := h.do(t, "DELETE", "/api/apps/app-1/sessions/"+sid, "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	// Re-GET should now 404 (session not found because state has FirstSeq=0).
	code, _ = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid, "user-A", "")
	if code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", code)
	}
}

func TestAPI_ListSessions_FiltersByUser(t *testing.T) {
	h := newAPIHarness(t)
	// Each listed session must be a real conversation : the list endpoint hides
	// empty shells (a session_started with no user message), so post one message
	// per session to make it non-empty.
	mkSession := func(user string) {
		_, body := h.do(t, "POST", "/api/apps/app-1/sessions", user, `{"title":"a"}`)
		var created map[string]any
		decodeBody(t, body, &created)
		sid := created["session_id"].(string)
		h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", user, `{"content":"hi"}`)
	}
	for i := 0; i < 3; i++ {
		mkSession("user-A")
	}
	for i := 0; i < 2; i++ {
		mkSession("user-B")
	}
	// An empty shell for user-A : created but never used. It must NOT be listed.
	h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"empty"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)

	code, body := h.do(t, "GET", "/api/apps/app-1/sessions", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	var resp map[string]any
	decodeBody(t, body, &resp)
	sessions, _ := resp["sessions"].([]any)
	// 3 real user-A conversations : user-B's are filtered out, and the empty shell
	// is excluded as having no conversation.
	if len(sessions) != 3 {
		t.Fatalf("user-A sessions: %d (want 3)", len(sessions))
	}
}

func TestAPI_GetMemoryAgentsQueue(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	for _, endpoint := range []string{"memory", "agents", "queue"} {
		code, body := h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/"+endpoint, "user-A", "")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", endpoint, code, string(body))
		}
	}
}

// ---------- Approvals ----------

func TestAPI_ResolveApproval_AppendsEvent(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	// Inject a pending approval via bus directly (the runtime would normally do this).
	h.bus.Append(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventApprovalRequest,
		SessionID: sid,
		UserID:    "user-A",
		Approval:  &sessionstore.ApprovalPayload{ID: "ap-1", Kind: "write_file"},
	})

	body2 := fmt.Sprintf(`{"session_id":"%s","approval_id":"ap-1","action":"grant"}`, sid)
	code, _ := h.do(t, "POST", "/api/apps/app-1/approve", "user-A", body2)
	if code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	state, _ := h.bus.State(sid)
	state.RLock()
	ap := state.Approvals["ap-1"]
	state.RUnlock()
	if ap == nil || ap.Status != "granted" {
		t.Fatalf("approval: %+v", ap)
	}
}

// ---------- Secrets ----------

func TestAPI_SecretsCRUD(t *testing.T) {
	h := newAPIHarness(t)

	// PUT one secret
	code, _ := h.do(t, "PUT", "/api/apps/app-1/secrets/openai_key", "user-A", `{"value":"sk-test"}`)
	if code != http.StatusOK {
		t.Fatalf("put one: %d", code)
	}

	// GET
	code, body := h.do(t, "GET", "/api/apps/app-1/secrets/openai_key", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	if !strings.Contains(string(body), `"value":"sk-test"`) {
		t.Fatalf("get body: %s", body)
	}

	// LIST (masked)
	code, body = h.do(t, "GET", "/api/apps/app-1/secrets", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if !strings.Contains(string(body), `"openai_key":"********"`) {
		t.Fatalf("list body: %s", body)
	}

	// PUT bulk
	code, _ = h.do(t, "PUT", "/api/apps/app-1/secrets", "user-A", `{"k1":"v1","k2":"v2"}`)
	if code != http.StatusOK {
		t.Fatalf("put bulk: %d", code)
	}

	// DELETE
	code, _ = h.do(t, "DELETE", "/api/apps/app-1/secrets/k1", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}

	// User isolation : user-B should not see user-A's secrets.
	code, body = h.do(t, "GET", "/api/apps/app-1/secrets", "user-B", "")
	if code != http.StatusOK {
		t.Fatalf("user-B list: %d", code)
	}
	var resp map[string]any
	decodeBody(t, body, &resp)
	if resp["count"].(float64) != 0 {
		t.Fatalf("LEAK : user-B sees secrets %v", resp)
	}
}

// ---------- Diagnostics ----------

func TestAPI_Diagnostics_ReturnsAllStats(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "GET", "/api/apps/app-1/diagnostics", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("diag: %d %s", code, string(body))
	}
	var resp map[string]any
	decodeBody(t, body, &resp)
	for _, key := range []string{"flusher", "bus", "bridge", "runtime_go", "instance_id"} {
		if resp[key] == nil {
			t.Errorf("diag missing key: %s", key)
		}
	}
}

func TestAPI_AppStatus_OK(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "GET", "/api/apps/app-1/status", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if !bytes.Contains(body, []byte(`"status":"running"`)) {
		t.Fatalf("status body: %s", body)
	}
}

func TestAPI_NotImplemented_ReturnsCleanShape(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/resume", "user-A", "")
	if code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", code)
	}
	var resp map[string]any
	decodeBody(t, body, &resp)
	if resp["error"] != "not_implemented" || resp["feature"] != "resume" {
		t.Fatalf("not_implemented shape: %+v", resp)
	}
}

// ---------- Liveness ----------

func TestAPI_HealthAndReady_Return200(t *testing.T) {
	h := newAPIHarness(t)
	// Re-mount /health on the harness mux (the test harness builds its
	// own router subset ; we mirror what MountAPI does for /health).
	h.mux.Get("/health", livenessHandler)
	h.mux.Get("/ready", livenessHandler)

	for _, path := range []string{"/health", "/ready"} {
		code, body := h.do(t, "GET", path, "", "")
		if code != http.StatusOK {
			t.Errorf("%s : code = %d, want 200 ; body=%s", path, code, body)
		}
		var resp map[string]any
		decodeBody(t, body, &resp)
		if resp["status"] != "ok" {
			t.Errorf("%s : body = %s, want status=ok", path, body)
		}
	}
}

// ---------- Runtime wiring (R-3) ----------

func TestAPI_PostMessage_KicksEngineWhenAvailable(t *testing.T) {
	h := newAPIHarness(t)
	runner := newFakeRunner()
	h.daemon.engine = runner

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
		`{"content":"hello"}`)
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, string(body))
	}

	runner.waitCalled(t, 2*time.Second)
	in, n := runner.snapshot()
	if n != 1 {
		t.Fatalf("engine called %d times, want 1", n)
	}
	if in.AppID != "app-1" || in.SessionID != sid {
		t.Errorf("TurnInput = %+v ; want app-1 / %s", in, sid)
	}
}

// TestAPI_PostMessage_ThreadsMode : the composer mode from the POST body
// reaches the engine via TurnInput.Mode.
func TestAPI_PostMessage_ThreadsMode(t *testing.T) {
	h := newAPIHarness(t)
	runner := newFakeRunner()
	h.daemon.engine = runner

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
		`{"content":"hello","mode":"plan"}`)
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, string(body))
	}

	runner.waitCalled(t, 2*time.Second)
	in, _ := runner.snapshot()
	if in.Mode != "plan" {
		t.Errorf("TurnInput.Mode = %q, want plan", in.Mode)
	}
}

// TestAPI_GetSession_ExposesActiveMode : after a mode-switch directive binds
// the session's active mode (the exact durable EventSystemMessage the engine's
// injectSystemDirective emits), getSession surfaces it as active_mode so the
// composer picker can restore the user's last-active mode on reload.
func TestAPI_GetSession_ExposesActiveMode(t *testing.T) {
	h := newAPIHarness(t)

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	// Fresh session : no mode bound yet.
	_, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid, "user-A", "")
	var before map[string]any
	decodeBody(t, body, &before)
	if got, _ := before["active_mode"].(string); got != "" {
		t.Fatalf("fresh session active_mode = %q, want empty", got)
	}

	// Persist the mode-switch directive verbatim (same shape as the engine).
	if _, err := h.bus.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventSystemMessage,
		SessionID: sid,
		AppID:     "app-1",
		UserID:    "user-A",
		Message: &sessionstore.MessagePayload{
			Role:    "system",
			Content: "[Mode: Plan] Planning only.",
			Extra: map[string]any{
				"source":   "mode_switch",
				"position": "append",
				"mode_id":  "plan",
			},
		},
	}); err != nil {
		t.Fatalf("append mode_switch: %v", err)
	}

	_, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid, "user-A", "")
	var after map[string]any
	decodeBody(t, body, &after)
	if got, _ := after["active_mode"].(string); got != "plan" {
		t.Errorf("active_mode = %q, want plan (session reload must restore bound mode)", got)
	}
}

func TestAPI_PostMessage_ForwardsBearerJWT(t *testing.T) {
	h := newAPIHarness(t)
	runner := newFakeRunner()
	h.daemon.engine = runner

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	req := httptest.NewRequest("POST", "/api/apps/app-1/sessions/"+sid+"/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("X-User-ID", "user-A")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-12345")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}

	runner.waitCalled(t, 2*time.Second)
	in, _ := runner.snapshot()
	if in.UserJWT != "tok-12345" {
		t.Errorf("UserJWT = %q, want tok-12345", in.UserJWT)
	}
}

func TestAPI_PostMessage_NonUserRoleSkipsEngine(t *testing.T) {
	h := newAPIHarness(t)
	runner := newFakeRunner()
	h.daemon.engine = runner

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
		`{"role":"system","content":"sys"}`)
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, string(body))
	}

	// Give any (incorrect) goroutine a chance to fire.
	time.Sleep(50 * time.Millisecond)
	if _, n := runner.snapshot(); n != 0 {
		t.Fatalf("engine fired %d times for non-user role ; expected 0", n)
	}
}

func TestAPI_PostMessage_NoEngine_StillReturns201(t *testing.T) {
	// Default harness has engine == nil. Verify graceful degradation.
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
		`{"content":"hi"}`)
	if code != http.StatusCreated {
		t.Fatalf("post with no engine should still persist: %d %s", code, string(body))
	}
}

func TestAPI_ConcurrentAppends_StableUnderLoad(t *testing.T) {
	h := newAPIHarness(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/messages", "user-A",
				fmt.Sprintf(`{"content":"m%d"}`, i))
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)

	state, _ := h.bus.State(sid)
	state.RLock()
	count := len(state.Messages)
	state.RUnlock()
	if count != N {
		t.Fatalf("concurrent appends : got %d messages, want %d", count, N)
	}
}
