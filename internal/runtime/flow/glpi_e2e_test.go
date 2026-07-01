package flow_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/catalog"
	glpihttp "github.com/digitornai/digitorn/internal/modules/http"
	"github.com/digitornai/digitorn/internal/runtime/flow"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/pkg/module"
)

// capturedReq records one inbound request the mock GLPI server received.
type capturedReq struct {
	method  string
	path    string
	headers http.Header
	body    string
}

// glpiE2ESink is the flow's durable session sink. It records every flow event
// AND auto-approves the human gate the instant the durable approval-request
// event lands — proving the gate is hit and that the request is appended
// durably (AppendDurable) before the flow proceeds.
type glpiE2ESink struct {
	mu        sync.Mutex
	events    []sessionstore.Event
	approvals *approval.Registry
	approve   string // the choice to grant when the gate fires
}

func (s *glpiE2ESink) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	n := uint64(len(s.events))
	s.mu.Unlock()
	if ev.Type == sessionstore.EventApprovalRequest && ev.Approval != nil {
		s.approvals.Resolve(ev.Approval.ID, approval.Resolution{
			Result: approval.ResultApproved,
			Reason: s.approve,
		})
	}
	return n, nil
}

func (s *glpiE2ESink) ranNode(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == sessionstore.EventFlowNodeEnd && e.Flow != nil && e.Flow.NodeID == nodeID {
			return true
		}
	}
	return false
}

func (s *glpiE2ESink) sawDurableApprovalRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == sessionstore.EventApprovalRequest && e.Approval != nil && e.Approval.Status == "pending" {
			return true
		}
	}
	return false
}

// TestGLPISupport_E2E_WritebackThroughApprovalGate is the flagship #2 proof:
// a faked GLPI "new ticket" event drives the REAL compiled glpi-support flow
// through triage → specialist → a human approval gate → a write-back to GLPI
// via the REAL http module (the GLPI REST API is mocked with httptest).
//
// Only the LLM agent is stubbed (deterministic, CI-safe) — everything else is
// production code: the flow runner, the approval registry/gate, and the http
// module (App-Token/Session-Token headers, SSRF guard, JSON body).
func TestGLPISupport_E2E_WritebackThroughApprovalGate(t *testing.T) {
	const ticketID = 4242

	// --- Mock GLPI REST endpoint -------------------------------------------
	var mu sync.Mutex
	var reqs []capturedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedReq{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(ticketID) + `}`))
	}))
	defer srv.Close()

	// Env the app references; resolved at compile time (non-strict EnvResolver
	// resolves present vars). The webhook key / openai key just need to exist.
	t.Setenv("GLPI_URL", srv.URL)
	t.Setenv("GLPI_APP_TOKEN", "app-tok-123")
	t.Setenv("GLPI_SESSION_TOKEN", "sess-tok-456")
	t.Setenv("GLPI_WEBHOOK_KEY", "hook-key-789")
	t.Setenv("OPENAI_API_KEY", "unused-in-this-test")

	// --- Compile the real flagship app -------------------------------------
	c := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})
	res, err := c.Compile("../../../examples/glpi-support")
	if err != nil {
		t.Fatalf("compile glpi-support: %v", err)
	}
	if !res.OK() {
		t.Fatalf("glpi-support must compile clean:\n%v", res.Diagnostics)
	}
	if res.Definition.Flow == nil {
		t.Fatal("glpi-support must define a flow")
	}

	// --- Real http module, configured from the COMPILED app config ----------
	// The App-Token/Session-Token headers were resolved from env at compile.
	// We only flip allow_private_hosts so the SSRF guard permits the loopback
	// httptest server.
	httpCfg := map[string]any{}
	if mb, ok := res.Definition.Tools.Modules["http"]; ok && mb.Config != nil {
		for k, v := range mb.Config {
			httpCfg[k] = v
		}
	}
	httpCfg["allow_private_hosts"] = true
	hm := glpihttp.New()
	if err := hm.Init(context.Background(), httpCfg); err != nil {
		t.Fatalf("http module init: %v", err)
	}

	// --- Flow deps ---------------------------------------------------------
	reg := approval.NewRegistry()
	sink := &glpiE2ESink{approvals: reg, approve: "approve"}

	runTool := func(ctx context.Context, inv flow.ToolInvocation) flow.ToolOutcome {
		mod, action, ok := strings.Cut(inv.Name, ".")
		if !ok || mod != "http" {
			return flow.ToolOutcome{Status: "errored", Error: "unexpected tool " + inv.Name}
		}
		argsJSON, _ := json.Marshal(inv.Args)
		r, err := hm.Invoke(ctx, action, argsJSON)
		if err != nil || !r.Success {
			msg := r.Error
			if err != nil {
				msg = err.Error()
			}
			return flow.ToolOutcome{Status: "errored", Error: msg}
		}
		out, _ := json.Marshal(r.Data)
		return flow.ToolOutcome{
			Status: "completed",
			Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: string(out)}},
		}
	}

	// Stubbed agent: triage classifies the ticket as "account"; the account
	// specialist returns a single JSON string literal (the app contract) so the
	// write-back body stays valid JSON under the flow's raw interpolation.
	const specialistReply = `"Hi! You can reset your password at https://reset.corp.example - the link expires after 1 hour. If the account is locked it auto-unlocks in 15 minutes."`
	var triageRan atomic.Bool
	runAgent := func(_ context.Context, spec flow.AgentSpec) (flow.AgentResult, error) {
		switch spec.AgentID {
		case "triage":
			triageRan.Store(true)
			return flow.AgentResult{Status: "completed", Content: `{"category":"account"}`}, nil
		case "account_expert":
			return flow.AgentResult{Status: "completed", Content: specialistReply}, nil
		default:
			return flow.AgentResult{Status: "completed", Content: `"(generic reply)"`}, nil
		}
	}

	var idn int64
	idGen := func() string { return "id-" + strconv.FormatInt(atomic.AddInt64(&idn, 1), 10) }

	deps := flow.Deps{
		Sessions:  sink,
		RunAgent:  runAgent,
		RunTool:   runTool,
		Approvals: reg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// --- Fake inbound GLPI "new ticket" event ------------------------------
	event := map[string]any{
		"payload": map[string]any{
			"id":                  ticketID,
			"status":              "new",
			"name":                "Cannot log in - forgot password",
			"itilcategories_name": "Account",
			"users_id":            7,
			"message":             "I forgot my password and now my account seems locked. Please help.",
		},
	}
	in := flow.RunInput("glpi-support", "ticket-4242", "user-7", "", "turn-1").WithEvent(event)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := flow.New(res.Definition.Flow, deps, idGen).Run(ctx, res.Definition.Flow, in)
	if err != nil {
		t.Fatalf("flow run: %v", err)
	}

	// --- Assertions: the flow traversed the intended path ------------------
	if !triageRan.Load() {
		t.Error("triage agent should have run")
	}
	for _, node := range []string{"triage_node", "account_node", "approval_gate", "glpi_followup", "glpi_resolve", "resolved"} {
		if !sink.ranNode(node) {
			t.Errorf("expected flow node %q to have run", node)
		}
	}
	if sink.ranNode("escalate") || sink.ranNode("writeback_failed") {
		t.Error("approved happy-path must not hit escalate/writeback_failed")
	}
	if !sink.sawDurableApprovalRequest() {
		t.Error("a durable approval request must be recorded before write-back (crash-safe gate)")
	}
	if !strings.Contains(result.Content, "4242") {
		t.Errorf("terminal output should reference the ticket id, got %q", result.Content)
	}

	// --- Assertions: GLPI received the right write-back calls --------------
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 GLPI calls (followup + resolve), got %d: %+v", len(reqs), reqs)
	}

	followup, resolve := reqs[0], reqs[1]

	if followup.method != http.MethodPost || followup.path != "/apirest.php/Ticket/4242/ITILFollowup" {
		t.Errorf("followup: got %s %s", followup.method, followup.path)
	}
	if got := followup.headers.Get("App-Token"); got != "app-tok-123" {
		t.Errorf("followup App-Token = %q, want app-tok-123 (compiled from env)", got)
	}
	if got := followup.headers.Get("Session-Token"); got != "sess-tok-456" {
		t.Errorf("followup Session-Token = %q, want sess-tok-456", got)
	}
	if !json.Valid([]byte(followup.body)) {
		t.Errorf("followup body must be valid JSON, got: %s", followup.body)
	}
	if !strings.Contains(followup.body, "reset.corp.example") {
		t.Errorf("followup body should carry the specialist's approved reply, got: %s", followup.body)
	}
	if !strings.Contains(followup.body, `"items_id":4242`) {
		t.Errorf("followup body should target ticket 4242, got: %s", followup.body)
	}

	if resolve.method != http.MethodPut || resolve.path != "/apirest.php/Ticket/4242" {
		t.Errorf("resolve: got %s %s", resolve.method, resolve.path)
	}
	if !strings.Contains(resolve.body, `"status":5`) {
		t.Errorf("resolve body should set status 5 (Solved), got: %s", resolve.body)
	}
}

// TestGLPISupport_E2E_RejectionSkipsWriteback proves the safety guarantee: when
// the human REJECTS at the gate, NO write-back to GLPI happens and the ticket is
// left for a human (escalate).
func TestGLPISupport_E2E_RejectionSkipsWriteback(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	t.Setenv("GLPI_URL", srv.URL)
	t.Setenv("GLPI_APP_TOKEN", "app-tok-123")
	t.Setenv("GLPI_SESSION_TOKEN", "sess-tok-456")
	t.Setenv("GLPI_WEBHOOK_KEY", "hook-key-789")
	t.Setenv("OPENAI_API_KEY", "unused-in-this-test")

	c := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})
	res, err := c.Compile("../../../examples/glpi-support")
	if err != nil || !res.OK() {
		t.Fatalf("compile glpi-support: %v\n%v", err, res.Diagnostics)
	}

	reg := approval.NewRegistry()
	// Reject at the gate.
	rejSink := &rejectingSink{approvals: reg}

	hm := glpihttp.New()
	_ = hm.Init(context.Background(), map[string]any{"allow_private_hosts": true})

	runTool := func(ctx context.Context, inv flow.ToolInvocation) flow.ToolOutcome {
		mod, action, _ := strings.Cut(inv.Name, ".")
		_ = action
		_ = mod
		argsJSON, _ := json.Marshal(inv.Args)
		r, _ := hm.Invoke(ctx, action, argsJSON)
		return flow.ToolOutcome{Status: "completed", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: r.Error}}}
	}
	runAgent := func(_ context.Context, spec flow.AgentSpec) (flow.AgentResult, error) {
		if spec.AgentID == "triage" {
			return flow.AgentResult{Status: "completed", Content: `{"category":"network"}`}, nil
		}
		return flow.AgentResult{Status: "completed", Content: `"some drafted reply"`}, nil
	}
	var idn int64
	idGen := func() string { return "id-" + strconv.FormatInt(atomic.AddInt64(&idn, 1), 10) }

	deps := flow.Deps{Sessions: rejSink, RunAgent: runAgent, RunTool: runTool, Approvals: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	event := map[string]any{"payload": map[string]any{"id": 99, "status": "new", "message": "vpn down", "itilcategories_name": "Network"}}
	in := flow.RunInput("glpi-support", "ticket-99", "user-1", "", "turn-1").WithEvent(event)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := flow.New(res.Definition.Flow, deps, idGen).Run(ctx, res.Definition.Flow, in); err != nil {
		t.Fatalf("flow run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("a REJECTED ticket must NOT call GLPI; got %d calls", calls)
	}
	if !rejSink.ranNode("escalate") {
		t.Error("a rejected ticket should route to escalate")
	}
}

type rejectingSink struct {
	mu        sync.Mutex
	events    []sessionstore.Event
	approvals *approval.Registry
}

func (s *rejectingSink) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	n := uint64(len(s.events))
	s.mu.Unlock()
	if ev.Type == sessionstore.EventApprovalRequest && ev.Approval != nil {
		s.approvals.Resolve(ev.Approval.ID, approval.Resolution{Result: approval.ResultDenied})
	}
	return n, nil
}

func (s *rejectingSink) ranNode(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == sessionstore.EventFlowNodeEnd && e.Flow != nil && e.Flow.NodeID == nodeID {
			return true
		}
	}
	return false
}
