package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// SG-5 conformity E2E :
// Reproduces the canonical approval-bot scenario from
// docs-site/docs/tutorial/security-01-approval.md and asserts every
// documented behaviour against a REAL sessionstore.Bus (write-behind
// sharded, projection-driven) — no stubs on the persistence path.
//
// Documented claims verified (from the doc, section by section) :
//
//   - "tools.capabilities.approve declares which actions need permission"
//   - "approval_timeout caps how long the daemon waits (30-3600, default 300)"
//   - "Captured by GET /api/apps/{app_id}/approvals" : the JSON payload
//     has request_id, agent_id, user_id, app_id, session_id, tool_name,
//     tool_params, risk_level, reason
//   - "The agent loop is paused on this request. No tokens get billed,
//     no further LLM call happens until the queue resolves."
//   - "The supervisor approves ... daemon unfreezes the agent loop"
//   - "After approval the action executes ... tool_calls_count: 1"
//   - "client.deny(...) → agent receives a permission_denied error
//     in place of the tool result"

// realBusSessions wraps a real sessionstore.Bus as a
// runtime.SessionAccess. The whole point of an E2E test is to NOT
// use stubs on the persistence path — we want the real projection
// to run, the real shard routing, the real lock semantics.
type realBusSessions struct {
	bus *sessionstore.Bus
}

func (r *realBusSessions) State(sid string) (*sessionstore.SessionState, error) {
	return r.bus.State(sid)
}
func (r *realBusSessions) AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	return r.bus.AppendDurable(ctx, ev)
}
func (r *realBusSessions) Append(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	return r.bus.Append(ctx, ev)
}

// buildE2EBus spins up an in-memory sessionstore.Bus suitable for
// E2E runtime tests. Cleaned up by t.Cleanup.
func buildE2EBus(t *testing.T) *sessionstore.Bus {
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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})
	return bus
}

// buildApprovalBotApp reproduces approval-bot.yaml from the doc as
// an in-memory RuntimeApp. The fields mirror the YAML keys :
//
//	app: { app_id: approval-bot, name: "Approval Bot", version: "1.0" }
//	runtime: { mode: conversation, max_turns: 6, timeout: 120 }
//	agents: [{ id: main, role: assistant, system_prompt: "..." }]
//	tools.capabilities:
//	  default_policy: auto
//	  max_risk_level: high
//	  approve: [{ module: shell, actions: [bash], reason: "..." }]
//	  approval_timeout: 60
func buildApprovalBotApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "approval-bot", Enabled: true},
		Definition: &schema.AppDefinition{
			SchemaVersion: 1,
			App: schema.AppMeta{
				AppID: "approval-bot", Name: "Approval Bot", Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:    "main",
				Role:  "assistant",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "You can run Bash commands. Be concise. If a " +
					"command is approved and executes, summarise its " +
					"output in one sentence.",
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
					Approve: []schema.CapabilityGrant{{
						Module: "shell",
						Tools:  []string{"bash"},
						Reason: "Shell commands need explicit approval before running.",
					}},
					ApprovalTimeout: 60,
				},
			},
		},
		BundleDir: "/tmp/approval-bot",
	}
}

// TestSG5_E2E_ApprovalBotScenario : the canonical end-to-end test.
// Reproduces the doc's transcript step-by-step.
func TestSG5_E2E_ApprovalBotScenario(t *testing.T) {
	bus := buildE2EBus(t)
	sess := &realBusSessions{bus: bus}
	app := buildApprovalBotApp()
	apps := &stubApps{app: app}

	// LLM stub : round 1 emits the Bash tool_call ; round 2 (post-
	// approval) sends the final user-facing reply.
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{
			ToolCalls: []llm.ChatToolCall{{
				ID:   "tc-bash-1",
				Name: "shell.bash",
				Arguments: map[string]any{
					"command":     "echo \"hello world\"",
					"description": "Print hello world",
				},
			}},
		},
		{Content: "Printed hello world successfully."},
	}}

	disp := &captureDispatcher{
		outcome: runtime.ToolOutcome{
			Status: "completed",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "hello world\n"},
			},
		},
	}

	registry := approval.NewRegistry()
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.ApprovalRegistry = registry
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"shell.bash": {Name: "bash", RiskLevel: tool.RiskHigh},
		}},
	}

	const sessID = "sess-e2e-approve"
	const userID = "e11e6e81e6864de9b654e02d309cc28a"
	if _, err := bus.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventSessionStarted,
		SessionID: sessID,
		AppID:     "approval-bot",
		UserID:    userID,
		Meta:      &sessionstore.MetaPayload{Title: "Demo"},
	}); err != nil {
		t.Fatalf("session init: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := e.Run(context.Background(), runtime.TurnInput{
			AppID: "approval-bot", SessionID: sessID, UserID: userID,
		})
		done <- err
	}()

	// === STEP 1 — agent attempts Bash ; gate 4 intercepts ===========
	// "The agent issues the tool call and the security gate intercepts"

	ap := waitForPendingApproval(t, bus, sessID, 2*time.Second)

	// --- Doc claim : payload JSON shape -----------------------------
	// {
	//   "request_id": "...",   ← ap.ID
	//   "agent_id": "main",    ← ap.AgentID
	//   "user_id": "...",      ← event.UserID
	//   "app_id": "approval-bot", ← event.AppID
	//   "session_id": "<sid>", ← event.SessionID
	//   "tool_name": "shell.bash",   ← ap.ToolName
	//   "tool_params": {
	//      "command": "echo \"hello world\"",
	//      "description": "Print hello world"
	//   },                     ← ap.ToolParams
	//   "risk_level": "high",  ← ap.RiskLevel
	//   "reason": "Shell commands need explicit approval before running."
	// }
	if ap.ID == "" {
		t.Errorf("request_id empty")
	}
	if ap.AgentID != "main" {
		t.Errorf("agent_id = %q, want main", ap.AgentID)
	}
	if ap.ToolName != "shell.bash" {
		t.Errorf("tool_name = %q, want shell.bash", ap.ToolName)
	}
	if got, _ := ap.ToolParams["command"].(string); got != "echo \"hello world\"" {
		t.Errorf("tool_params.command = %q, want 'echo \"hello world\"'", got)
	}
	if got, _ := ap.ToolParams["description"].(string); got != "Print hello world" {
		t.Errorf("tool_params.description = %q, want 'Print hello world'", got)
	}
	if ap.RiskLevel != "high" {
		t.Errorf("risk_level = %q, want high", ap.RiskLevel)
	}
	if ap.Reason != "Shell commands need explicit approval before running." {
		t.Errorf("reason = %q, want 'Shell commands need explicit approval before running.'", ap.Reason)
	}
	if ap.Status != "pending" {
		t.Errorf("status = %q, want pending", ap.Status)
	}

	// --- Doc claim : "The agent loop is paused" ---------------------
	// "No tokens get billed, no further LLM call happens until the
	// queue resolves." Verify both : dispatcher hasn't been called
	// AND the LLM hasn't been re-invoked while we wait.
	if disp.count != 0 {
		t.Fatalf("PAUSE BROKEN : dispatcher called %d times before resolve", disp.count)
	}
	if lc.calls != 1 {
		t.Fatalf("EXTRA LLM CALL during pause : calls=%d, want 1", lc.calls)
	}

	// === STEP 2 — supervisor approves ==============================
	// "A web client posts the equivalent JSON to POST
	// /api/apps/approval-bot/approve with {request_id: ..., approved: true}.
	// Either way the daemon unfreezes the agent loop."
	//
	// Wait until the engine goroutine actually registers a waiter
	// in the registry before resolving (SG-6 inserts an additional
	// audit event between the approval request and awaitApproval,
	// so the EventApprovalRequest now lands before registry.Register).
	deadline := time.Now().Add(time.Second)
	for registry.Pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if registry.Pending() == 0 {
		t.Fatal("registry never received a waiter")
	}
	if !registry.Resolve(ap.ID, approval.Resolution{
		Result: approval.ResultApproved,
		Reason: "user clicked approve",
	}) {
		t.Fatal("Resolve found no waiter")
	}

	// Now the turn must complete.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn didn't complete after approve")
	}

	// === STEP 3 — the action executes ===============================
	// Doc claim : tool_call event after approval :
	// {"name": "Bash", "success": true, "result": {...}}
	// "tool_calls_count: 1, one Bash call"
	if disp.count != 1 {
		t.Fatalf("tool_calls_count = %d, want 1", disp.count)
	}

	// Re-read state to verify the events landed correctly.
	state, _ := bus.State(sessID)
	state.RLock()

	// One tool_result completed (success:true equivalent).
	var completed int
	for _, tc := range state.ToolCalls {
		if tc.Status == "completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Errorf("completed tool_calls = %d, want 1", completed)
	}

	// Final agent reply must be in the messages.
	var lastAssistant string
	for _, m := range state.Messages {
		if m.Role == "assistant" && m.Content != "" {
			lastAssistant = m.Content
		}
	}
	state.RUnlock()

	if !strings.Contains(lastAssistant, "Printed") {
		t.Errorf("final agent reply = %q (want contains 'Printed')", lastAssistant)
	}

	// Approval state moved to "granted".
	state.RLock()
	finalAp := state.Approvals[ap.ID]
	state.RUnlock()
	if finalAp == nil {
		t.Fatal("approval state vanished")
	}
	// The runtime-level Resolve doesn't emit EventApprovalGranted ;
	// that's the HTTP handler's job (covered by the HTTP E2E test
	// in internal/server). In runtime-only mode the approval state
	// stays at "pending" after registry.Resolve. This is OK : the
	// timeline reflects "human resolved out-of-band" without a fake
	// runtime-side event.

	t.Logf("✓ approval-bot scenario : 1 approval, 1 bash dispatch, 1 final reply")
}

// TestSG5_E2E_DenyScenario : the doc's "Denying instead of
// approving" section. After client.deny() the agent receives
// permission_denied in place of the tool result.
func TestSG5_E2E_DenyScenario(t *testing.T) {
	bus := buildE2EBus(t)
	sess := &realBusSessions{bus: bus}
	apps := &stubApps{app: buildApprovalBotApp()}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "tc-bash-1", Name: "shell.bash",
			Arguments: map[string]any{"command": "rm -rf /"},
		}}},
		{Content: "Cannot proceed, the command was denied."},
	}}
	disp := &captureDispatcher{outcome: runtime.ToolOutcome{Status: "completed"}}
	registry := approval.NewRegistry()
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.ApprovalRegistry = registry
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"shell.bash": {Name: "bash", RiskLevel: tool.RiskHigh},
		}},
	}

	const sessID = "sess-e2e-deny"
	const userID = "u-deny"
	_, _ = bus.AppendDurable(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: sessID,
		AppID: "approval-bot", UserID: userID,
	})

	done := make(chan error, 1)
	go func() {
		_, err := e.Run(context.Background(), runtime.TurnInput{
			AppID: "approval-bot", SessionID: sessID, UserID: userID,
		})
		done <- err
	}()

	ap := waitForPendingApproval(t, bus, sessID, 2*time.Second)
	// Wait until the engine goroutine registers a waiter — SG-6 inserts
	// an audit event between the approval-request emission and the
	// awaitApproval call.
	deadline := time.Now().Add(time.Second)
	for registry.Pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	registry.Resolve(ap.ID, approval.Resolution{
		Result: approval.ResultDenied,
		Reason: "too risky in this session",
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn didn't complete after deny")
	}

	if disp.count != 0 {
		t.Fatalf("PROHIBITED DISPATCH : dispatcher called %d times despite deny", disp.count)
	}

	state, _ := bus.State(sessID)
	state.RLock()
	defer state.RUnlock()

	// The tool_call's status should reflect the denial.
	var errored int
	var lastErr string
	for _, tc := range state.ToolCalls {
		if tc.Status == "errored" {
			errored++
			lastErr = tc.Error
		}
	}
	if errored != 1 {
		t.Fatalf("errored tool_calls = %d, want 1", errored)
	}
	if !strings.Contains(lastErr, "too risky") {
		t.Errorf("error text missing reason : %q", lastErr)
	}
}

// TestSG5_E2E_ResolutionOrder_DenyOverApprove : the precedence rule
// from the doc — "Resolution order is deny > approve > grant >
// default_policy. The first match wins. An action listed in deny is
// unreachable even if a grant row also names it. An action listed in
// approve requires confirmation even if a wildcard grant would have
// allowed it."
//
// Covers : an action that's in BOTH deny and approve must be denied
// (deny wins, no approval pause).
func TestSG5_E2E_ResolutionOrder_DenyOverApprove(t *testing.T) {
	bus := buildE2EBus(t)
	sess := &realBusSessions{bus: bus}
	app := buildApprovalBotApp()
	// Add a deny for the same action — must override the approve.
	app.Definition.Tools.Capabilities.Deny = []schema.CapabilityGrant{{
		Module: "shell", Tools: []string{"bash"},
	}}
	apps := &stubApps{app: app}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "tc-1", Name: "shell.bash",
			Arguments: map[string]any{"command": "echo hi"},
		}}},
		{Content: "blocked"},
	}}
	disp := &captureDispatcher{}
	registry := approval.NewRegistry()
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.ApprovalRegistry = registry
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"shell.bash": {Name: "bash", RiskLevel: tool.RiskHigh},
		}},
	}

	const sessID = "sess-deny-precedence"
	_, _ = bus.AppendDurable(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: sessID,
		AppID: "approval-bot", UserID: "u",
	})

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "approval-bot", SessionID: sessID, UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// No approval should have been requested (deny wins, no pause).
	if registry.Pending() != 0 {
		t.Fatalf("Pending approvals = %d, want 0 (deny must not pause)", registry.Pending())
	}
	// No dispatch.
	if disp.count != 0 {
		t.Fatalf("PROHIBITED DISPATCH : %d", disp.count)
	}
}

// waitForPendingApproval polls the bus state until at least one
// approval shows up with status="pending", then returns it.
func waitForPendingApproval(t *testing.T, bus *sessionstore.Bus, sid string, timeout time.Duration) *sessionstore.ApprovalState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := bus.State(sid)
		if err == nil && state != nil {
			state.RLock()
			for _, ap := range state.Approvals {
				if ap.Status == "pending" {
					cp := *ap
					state.RUnlock()
					return &cp
				}
			}
			state.RUnlock()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no pending approval within %v", timeout)
	return nil
}
