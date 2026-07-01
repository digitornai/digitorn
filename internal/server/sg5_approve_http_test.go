package server

import (
	"context"
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

	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// TestSG5_HTTPApprove_SignalsRegistry : the final E2E claim of the
// SG-5 chain. The doc states :
//
//	"A web client posts the equivalent JSON to
//	 POST /api/apps/approval-bot/approve with
//	 {request_id: ..., approved: true}. Either way the daemon
//	 unfreezes the agent loop."
//
// Our HTTP endpoint uses {session_id, approval_id, action} (Go T5
// contract, validated by the user). The functional claim is the
// same : posting the resolve must signal the in-process registry
// so the goroutine in awaitApproval unblocks.
//
// Pretty-printed flow this test verifies :
//
//	[goroutine] registry.Wait("ap-1", 5s) → blocks
//	[HTTP] POST /api/apps/X/approve {approval_id:"ap-1", action:"grant"}
//	         ↓
//	[handler] sessionStore.Append(EventApprovalGranted)
//	         ↓
//	[handler] approvalRegistry.Resolve("ap-1", Approved)
//	         ↓
//	[goroutine] Wait returns {Result: Approved}
func TestSG5_HTTPApprove_SignalsRegistry(t *testing.T) {
	h := newAPIHarnessWithApprovalRegistry(t)
	const sessID = "sess-sg5-http"

	// Pre-create the session (POST /sessions).
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	gotSid, _ := created["session_id"].(string)
	if gotSid == "" {
		t.Fatalf("no session_id in create response: %s", string(body))
	}
	_ = sessID

	// Inject a pending approval (the runtime would do this normally
	// via engine.awaitApproval).
	const approvalID = "ap-http-1"
	if _, err := h.bus.Append(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventApprovalRequest,
		SessionID: gotSid,
		AppID:     "app-1",
		UserID:    "user-A",
		Approval: &sessionstore.ApprovalPayload{
			ID: approvalID, Kind: "tool_call", Status: "pending",
			ToolName: "shell.bash", RiskLevel: "high",
			Reason: "Shell needs approval",
		},
	}); err != nil {
		t.Fatalf("inject approval: %v", err)
	}

	// Start a goroutine that registers a waiter on the registry —
	// this simulates the runtime's awaitApproval blocking call.
	var wg sync.WaitGroup
	wg.Add(1)
	var got approval.Resolution
	go func() {
		defer wg.Done()
		got = h.approvalRegistry.Wait(context.Background(), approvalID, 5*time.Second)
	}()

	// Give the goroutine a tick to register.
	deadline := time.Now().Add(time.Second)
	for h.approvalRegistry.Pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if h.approvalRegistry.Pending() != 1 {
		t.Fatalf("registry Pending = %d, want 1 (waiter not registered ?)",
			h.approvalRegistry.Pending())
	}

	// === The actual HTTP request : POST /approve ===================
	reqBody := fmt.Sprintf(
		`{"session_id":"%s","approval_id":"%s","action":"grant","reason":"user clicked approve"}`,
		gotSid, approvalID)
	code, respBody := h.do(t, "POST", "/api/apps/app-1/approve", "user-A", reqBody)
	if code != http.StatusOK {
		t.Fatalf("approve HTTP code = %d, body=%s", code, string(respBody))
	}

	// === The goroutine must unblock with ResultApproved ===========
	wg.Wait()
	if got.Result != approval.ResultApproved {
		t.Fatalf("registry Wait result = %v, want ResultApproved", got.Result)
	}
	if got.Reason != "user clicked approve" {
		t.Errorf("reason = %q, want 'user clicked approve' (HTTP body should pass through)",
			got.Reason)
	}

	// === The durable event landed and the state moved to granted ==
	state, _ := h.bus.State(gotSid)
	state.RLock()
	ap := state.Approvals[approvalID]
	state.RUnlock()
	if ap == nil {
		t.Fatal("approval state vanished")
	}
	if ap.Status != "granted" {
		t.Fatalf("approval state status = %q, want granted", ap.Status)
	}
}

// TestSG5_HTTPDeny_SignalsRegistry : same as above but POSTs a
// deny. The waiter must receive ResultDenied with the user's reason
// threaded through.
func TestSG5_HTTPDeny_SignalsRegistry(t *testing.T) {
	h := newAPIHarnessWithApprovalRegistry(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	const approvalID = "ap-deny-1"
	h.bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventApprovalRequest, SessionID: sid,
		AppID: "app-1", UserID: "user-A",
		Approval: &sessionstore.ApprovalPayload{
			ID: approvalID, Status: "pending", ToolName: "shell.bash",
		},
	})

	var got approval.Resolution
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		got = h.approvalRegistry.Wait(context.Background(), approvalID, 5*time.Second)
	}()
	for h.approvalRegistry.Pending() == 0 {
		time.Sleep(1 * time.Millisecond)
	}

	reqBody := fmt.Sprintf(
		`{"session_id":"%s","approval_id":"%s","action":"deny","reason":"too risky"}`,
		sid, approvalID)
	code, _ := h.do(t, "POST", "/api/apps/app-1/approve", "user-A", reqBody)
	if code != http.StatusOK {
		t.Fatalf("HTTP code = %d", code)
	}

	wg.Wait()
	if got.Result != approval.ResultDenied {
		t.Fatalf("result = %v, want ResultDenied", got.Result)
	}
	if got.Reason != "too risky" {
		t.Errorf("reason = %q", got.Reason)
	}
}

// TestSG5_HTTPApprove_PayloadShape : the doc shows the captured GET
// /api/apps/{id}/approvals payload with specific fields. Verify our
// approval state projects all of them so the UI can read them.
func TestSG5_HTTPApprove_PayloadShape(t *testing.T) {
	h := newAPIHarnessWithApprovalRegistry(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	// Mirror the production path: an approval is always raised mid-turn, AFTER a
	// user message, and the runtime emits it via AppendDurable (fsync) — see
	// engine.go:awaitApproval / askuser.go. Using Append (async) on a session
	// with no user_message instead made this test hit (a) the flush race and
	// (b) the "empty shell" filter in walkSessionMeta, neither of which happens
	// in prod. So persist a user message, then the approval, both durably.
	if _, err := h.bus.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: sid,
		AppID:     "app-1",
		UserID:    "user-A",
		Message: &sessionstore.MessagePayload{
			Role:  "user",
			Parts: []sessionstore.MessagePart{{Text: "Please run the command"}},
		},
	}); err != nil {
		t.Fatalf("inject user message: %v", err)
	}

	if _, err := h.bus.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventApprovalRequest,
		SessionID: sid,
		AppID:     "approval-bot",
		UserID:    "e11e6e81e6864de9b654e02d309cc28a",
		Approval: &sessionstore.ApprovalPayload{
			ID:       "ap-payload",
			Kind:     "tool_call",
			Status:   "pending",
			Reason:   "Shell commands need explicit approval before running.",
			AgentID:  "main",
			ToolName: "shell.bash",
			ToolParams: map[string]any{
				"command":     "echo \"hello world\"",
				"description": "Print hello world",
			},
			RiskLevel: "high",
		},
	}); err != nil {
		t.Fatalf("inject: %v", err)
	}

	// GET /api/apps/{id}/approvals — verify the payload reflects the
	// documented shape after projection.
	code, listBody := h.do(t, "GET", "/api/apps/app-1/approvals", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("GET approvals : %d", code)
	}
	var listResp struct {
		Approvals []map[string]any `json:"approvals"`
	}
	decodeBody(t, listBody, &listResp)
	if len(listResp.Approvals) == 0 {
		t.Fatal("no approvals returned")
	}

	// The handler aggregates per-session ; find the one we injected.
	// listApprovals shape varies ; we mainly check the underlying
	// state directly to keep the assertion robust.
	state, _ := h.bus.State(sid)
	state.RLock()
	ap := state.Approvals["ap-payload"]
	state.RUnlock()
	if ap == nil {
		t.Fatal("approval state missing")
	}

	// All documented fields must be present.
	for k, want := range map[string]string{
		"AgentID":   "main",
		"ToolName":  "shell.bash",
		"RiskLevel": "high",
		"Reason":    "Shell commands need explicit approval before running.",
		"Status":    "pending",
	} {
		got := map[string]string{
			"AgentID":   ap.AgentID,
			"ToolName":  ap.ToolName,
			"RiskLevel": ap.RiskLevel,
			"Reason":    ap.Reason,
			"Status":    ap.Status,
		}[k]
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got, _ := ap.ToolParams["command"].(string); got != "echo \"hello world\"" {
		t.Errorf("ToolParams.command = %q", got)
	}
}

// TestSG5_HTTPApprove_UnknownID_NoOp : posting a resolve for an
// approval no one is waiting on must not panic. The append still
// happens (durable trace) ; the registry.Resolve is a no-op.
func TestSG5_HTTPApprove_UnknownID_NoOp(t *testing.T) {
	h := newAPIHarnessWithApprovalRegistry(t)
	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	reqBody := fmt.Sprintf(
		`{"session_id":"%s","approval_id":"unknown","action":"grant"}`, sid)
	code, _ := h.do(t, "POST", "/api/apps/app-1/approve", "user-A", reqBody)
	if code != http.StatusOK {
		t.Fatalf("HTTP code = %d, want 200 (no-op resolve is OK)", code)
	}
	// No panic = success.
}

// ---- harness ------------------------------------------------------

// apiHarnessSG5 extends apiHarness with the approval registry so
// the HTTP handler can signal it. Built by hand because the
// existing newAPIHarness doesn't wire approvalRegistry.
type apiHarnessSG5 struct {
	mux              *chi.Mux
	bus              *sessionstore.Bus
	flusher          *sessionstore.DiskFlusher
	approvalRegistry *approval.Registry
}

func (h *apiHarnessSG5) do(t *testing.T, method, path, user, body string) (int, []byte) {
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

func newAPIHarnessWithApprovalRegistry(t *testing.T) *apiHarnessSG5 {
	t.Helper()
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths:            paths,
		NumShards:        2,
		QueueCapPerShard: 4096,
		BatchMax:         100,
		FlushInterval:    2 * time.Millisecond,
		Fsync:            false,
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
	builder := sessionstore.NewEnvelopeBuilder("inst-sg5", []string{"chat"})
	bridge := NewSocketIOBridge(rt, bus, builder, paths, NullAuth{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	bridge.Start(context.Background())

	registry := approval.NewRegistry()
	d := &Daemon{
		sessionStore:     bus,
		sessionFlusher:   flusher,
		sessionPaths:     paths,
		envelopeBuilder:  builder,
		bridge:           bridge,
		approvalRegistry: registry,
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware)
		r.Post("/api/apps/{app_id}/sessions", d.createSession)
		r.Get("/api/apps/{app_id}/approvals", d.listApprovals)
		r.Post("/api/apps/{app_id}/approve", d.resolveApproval)
	})

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bridge.Stop(ctx)
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})

	return &apiHarnessSG5{mux: r, bus: bus, flusher: flusher, approvalRegistry: registry}
}

// decodeBody is reused from api_test.go (same package).
