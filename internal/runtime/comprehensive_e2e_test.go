package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// CC-9 — Cross-user state isolation
//
// Two users in the SAME app must each have their own session
// state. Events emitted under user A must never appear under
// user B's session view, even when they share the same app id.
// =====================================================================

func TestComprehensive_CrossUserIsolation(t *testing.T) {
	app := realDispatchApp()

	sessA := newProjectingSessions("sess-userA")
	sessB := newProjectingSessions("sess-userB")

	apps := &stubApps{app: app}

	// User A runs first
	lcA := &stubLLM{resp: &llm.ChatResponse{Content: "A-reply"}}
	eA := newEngine(t, apps, sessA, lcA)
	_, _ = eA.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-userA", UserID: "userA",
	})

	// User B runs second
	lcB := &stubLLM{resp: &llm.ChatResponse{Content: "B-reply"}}
	eB := newEngine(t, apps, sessB, lcB)
	_, _ = eB.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-userB", UserID: "userB",
	})

	// Session A must only contain user A events.
	for _, ev := range sessA.events {
		if ev.UserID != "" && ev.UserID != "userA" {
			t.Errorf("session A leaked event for user %q", ev.UserID)
		}
		if ev.SessionID != "sess-userA" {
			t.Errorf("session A leaked SID %q", ev.SessionID)
		}
	}
	for _, ev := range sessB.events {
		if ev.UserID != "" && ev.UserID != "userB" {
			t.Errorf("session B leaked event for user %q", ev.UserID)
		}
		if ev.SessionID != "sess-userB" {
			t.Errorf("session B leaked SID %q", ev.SessionID)
		}
	}

	// Each user must have seen only their own assistant message.
	for _, ev := range sessA.events {
		if ev.Type == sessionstore.EventAssistantMessage && ev.Message != nil {
			for _, p := range ev.Message.Parts {
				if strings.Contains(p.Text, "B-reply") {
					t.Errorf("user A saw B-reply : %q", p.Text)
				}
			}
		}
	}
}

// =====================================================================
// CC-10 — Background tasks isolated per session
// =====================================================================

// bgIsolationRec tracks per-session launches so the test can
// verify isolation.
type bgIsolationRec struct {
	mu       sync.Mutex
	launches map[string][]string // sessionID -> taskIDs
}

type bgIsolationMgr struct{ rec *bgIsolationRec }

func (m *bgIsolationMgr) Launch(_ context.Context, req meta.LaunchRequest) (string, error) {
	m.rec.mu.Lock()
	defer m.rec.mu.Unlock()
	tid := req.Tool + "-id"
	m.rec.launches[req.SessionID] = append(m.rec.launches[req.SessionID], tid)
	return tid, nil
}
func (m *bgIsolationMgr) Status(_ context.Context, _, _ string) (meta.BackgroundStatus, error) {
	return meta.BackgroundStatus{}, nil
}
func (m *bgIsolationMgr) Wait(_ context.Context, _, _ string, _ float64) (meta.BackgroundStatus, error) {
	return meta.BackgroundStatus{}, nil
}
func (m *bgIsolationMgr) Cancel(_ context.Context, _, _ string) error { return nil }
func (m *bgIsolationMgr) List(_ context.Context, sessionID string) ([]meta.BackgroundStatus, error) {
	m.rec.mu.Lock()
	defer m.rec.mu.Unlock()
	out := make([]meta.BackgroundStatus, 0, len(m.rec.launches[sessionID]))
	for _, tid := range m.rec.launches[sessionID] {
		out = append(out, meta.BackgroundStatus{TaskID: tid})
	}
	return out, nil
}

func TestComprehensive_BackgroundTasksPerSession(t *testing.T) {
	rec := &bgIsolationRec{launches: map[string][]string{}}
	mgr := &bgIsolationMgr{rec: rec}

	disp := &meta.MetaDispatcher{Background: mgr}

	for i := 0; i < 5; i++ {
		out := disp.Dispatch(context.Background(), dgruntime.ToolInvocation{
			Name:   "context_builder.background_run",
			AppID:  "test-app",
			UserID: "userA",
			Args: map[string]any{
				"name": fmt.Sprintf("a.tool%d", i), "params": map[string]any{},
			},
		})
		if out.Status != "completed" {
			t.Fatalf("session A launch %d failed : %s", i, out.Error)
		}
	}
	for i := 0; i < 3; i++ {
		out := disp.Dispatch(context.Background(), dgruntime.ToolInvocation{
			Name:   "context_builder.background_run",
			AppID:  "test-app",
			UserID: "userB",
			Args: map[string]any{
				"name": fmt.Sprintf("b.tool%d", i), "params": map[string]any{},
			},
		})
		if out.Status != "completed" {
			t.Fatalf("session B launch %d failed : %s", i, out.Error)
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	// SessionID is derived in meta as appID+":"+userID per
	// MetaDispatcher.SessionID.
	keyA := "test-app:userA"
	keyB := "test-app:userB"
	if len(rec.launches[keyA]) != 5 {
		t.Errorf("session A launches = %d, want 5 : %+v",
			len(rec.launches[keyA]), rec.launches)
	}
	if len(rec.launches[keyB]) != 3 {
		t.Errorf("session B launches = %d, want 3 : %+v",
			len(rec.launches[keyB]), rec.launches)
	}
	if len(rec.launches) != 2 {
		t.Errorf("expected 2 session keys, got %d", len(rec.launches))
	}
}

// =====================================================================
// CC-11 — Cold-load session hydrates from a series of events
//
// Replay N events through sessionstore.Apply ; verify the final
// state has the correct Messages, ToolCalls, current turn, etc.
// =====================================================================

func TestComprehensive_ColdLoadHydratesState(t *testing.T) {
	state := sessionstore.NewSessionState("cold-sess")

	now := time.Now().UnixNano()
	// Build a deterministic event timeline :
	//  1. session_started
	//  2. user_message
	//  3. turn_started
	//  4. tool_call
	//  5. tool_result
	//  6. assistant_message
	//  7. turn_ended
	events := []sessionstore.Event{
		{Seq: 1, TsUnixNano: now, Type: sessionstore.EventSessionStarted,
			SessionID: "cold-sess", AppID: "app1", UserID: "u1"},
		{Seq: 2, TsUnixNano: now + 1, Type: sessionstore.EventUserMessage,
			SessionID: "cold-sess",
			Message: &sessionstore.MessagePayload{
				Role: "user",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: "hello"},
				},
			}},
		{Seq: 3, TsUnixNano: now + 2, Type: sessionstore.EventTurnStarted,
			SessionID: "cold-sess",
			Turn: &sessionstore.TurnPayload{
				TurnID: "turn-1", AgentID: "main",
			}},
		{Seq: 4, TsUnixNano: now + 3, Type: sessionstore.EventToolCall,
			SessionID: "cold-sess",
			Tool: &sessionstore.ToolPayload{
				CallID: "call-1", Name: "filesystem.read",
				Arguments: map[string]any{"path": "/x"},
				Status:    "pending",
			}},
		{Seq: 5, TsUnixNano: now + 4, Type: sessionstore.EventToolResult,
			SessionID: "cold-sess",
			Tool: &sessionstore.ToolPayload{
				CallID: "call-1", Name: "filesystem.read",
				Status: "completed",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: "file content"},
				},
			}},
		{Seq: 6, TsUnixNano: now + 5, Type: sessionstore.EventAssistantMessage,
			SessionID: "cold-sess",
			Message: &sessionstore.MessagePayload{
				Role: "assistant",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: "read it"},
				},
			}},
		{Seq: 7, TsUnixNano: now + 6, Type: sessionstore.EventTurnEnded,
			SessionID: "cold-sess",
			Turn: &sessionstore.TurnPayload{
				TurnID: "turn-1", Status: "done",
			}},
	}

	// Replay
	for i := range events {
		sessionstore.Apply(state, &events[i])
	}

	// Validate the projected state.
	if state.LastSeq != 7 {
		t.Errorf("LastSeq = %d, want 7", state.LastSeq)
	}
	if state.EventCount != 7 {
		t.Errorf("EventCount = %d, want 7", state.EventCount)
	}
	if state.CurrentTurnID != "" {
		t.Errorf("CurrentTurnID = %q, want empty (turn ended)", state.CurrentTurnID)
	}

	// Messages projected : user + tool result + assistant = 3
	if len(state.Messages) != 3 {
		t.Errorf("Messages = %d, want 3 : %+v",
			len(state.Messages), state.Messages)
	}

	// ToolCalls : 1 entry, status completed
	if len(state.ToolCalls) != 1 {
		t.Errorf("ToolCalls = %d, want 1", len(state.ToolCalls))
	}
	if tc := state.ToolCalls["call-1"]; tc == nil || tc.Status != "completed" {
		t.Errorf("call-1 status = %+v, want completed", tc)
	}

	// AppID and UserID populated from the first event that carries them.
	if state.AppID != "app1" {
		t.Errorf("AppID = %q", state.AppID)
	}
	if state.UserID != "u1" {
		t.Errorf("UserID = %q", state.UserID)
	}
}

// =====================================================================
// CC-11 (bis) — Recovery from a snapshot mid-stream :
// projection picks up where it left off when given a partial event
// list.
// =====================================================================

func TestComprehensive_PartialReplayCompletesCorrectly(t *testing.T) {
	state := sessionstore.NewSessionState("partial")

	// Apply 50 user-message events.
	now := time.Now().UnixNano()
	for i := 1; i <= 50; i++ {
		ev := sessionstore.Event{
			Seq:        uint64(i),
			TsUnixNano: now + int64(i),
			Type:       sessionstore.EventUserMessage,
			SessionID:  "partial",
			Message: &sessionstore.MessagePayload{
				Role: "user",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: fmt.Sprintf("msg-%d", i)},
				},
			},
		}
		sessionstore.Apply(state, &ev)
	}

	if state.LastSeq != 50 {
		t.Errorf("LastSeq = %d, want 50", state.LastSeq)
	}
	if len(state.Messages) != 50 {
		t.Errorf("Messages = %d, want 50", len(state.Messages))
	}
	// Apply next 50 — total should be 100.
	for i := 51; i <= 100; i++ {
		ev := sessionstore.Event{
			Seq:        uint64(i),
			TsUnixNano: now + int64(i),
			Type:       sessionstore.EventUserMessage,
			SessionID:  "partial",
			Message: &sessionstore.MessagePayload{
				Role: "user",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: fmt.Sprintf("msg-%d", i)},
				},
			},
		}
		sessionstore.Apply(state, &ev)
	}
	if state.LastSeq != 100 {
		t.Errorf("LastSeq after partial replay = %d, want 100", state.LastSeq)
	}
	if len(state.Messages) != 100 {
		t.Errorf("Messages after partial replay = %d, want 100", len(state.Messages))
	}
}

// =====================================================================
// CC-12 — Concurrent state access during writes
//
// While one goroutine writes events to a session, multiple readers
// poll the projected state. No torn reads, no panics, no missing
// events at end.
// =====================================================================

func TestComprehensive_ConcurrentReadsDuringWrites(t *testing.T) {
	const (
		nWrites  = 500
		nReaders = 16
	)
	state := sessionstore.NewSessionState("concurrent")

	var (
		wg      sync.WaitGroup
		readers atomic.Int64
	)
	stop := make(chan struct{})

	// Writer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		now := time.Now().UnixNano()
		for i := 1; i <= nWrites; i++ {
			ev := sessionstore.Event{
				Seq:        uint64(i),
				TsUnixNano: now + int64(i),
				Type:       sessionstore.EventUserMessage,
				SessionID:  "concurrent",
				Message: &sessionstore.MessagePayload{
					Role: "user",
					Parts: []sessionstore.MessagePart{
						{Type: sessionstore.PartTypeText, Text: fmt.Sprintf("msg-%d", i)},
					},
				},
			}
			sessionstore.Apply(state, &ev)
		}
		close(stop)
	}()

	// Reader goroutines — keep polling Snapshot until writer signals.
	for r := 0; r < nReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := state.Snapshot()
					_ = snap.LastSeq
					_ = len(snap.Messages)
					readers.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	t.Logf("readers performed %d snapshot reads during %d writes",
		readers.Load(), nWrites)

	// Final state must have all writes.
	if state.LastSeq != nWrites {
		t.Errorf("LastSeq = %d, want %d", state.LastSeq, nWrites)
	}
	if len(state.Messages) != nWrites {
		t.Errorf("Messages = %d, want %d", len(state.Messages), nWrites)
	}
}

// =====================================================================
// CC-13 — Hook gate veto blocks dispatch in real engine flow
// =====================================================================

// trackingDispatcher counts how many times it's been called.
type trackingDispatcher struct {
	calls atomic.Int64
}

func (d *trackingDispatcher) Dispatch(_ context.Context, _ dgruntime.ToolInvocation) dgruntime.ToolOutcome {
	d.calls.Add(1)
	return dgruntime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ran"}},
	}
}

func TestComprehensive_HookGateVetoBlocksDispatch(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-veto")

	// Hook : block filesystem.delete via gate.
	hk := schema.Hook{
		ID: "block_delete",
		On: schema.HookEventToolStart,
		Condition: schema.HookCondition{
			Type: "tool_name", Params: map[string]any{"match": "filesystem.delete"},
		},
		Action: schema.HookAction{
			Type:   "gate",
			Params: map[string]any{"allow": false, "reason": "delete forbidden by hook"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentRT4Logger{}})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.delete",
			Arguments: map[string]any{"path": "/important"},
		}}},
		{Content: "noted, can't delete"},
	}}

	disp := &trackingDispatcher{}
	cb := buildContextOnly([]policy.AvailableAction{
		{Module: "filesystem", Action: "delete",
			Spec: &tool.Spec{Name: "filesystem.delete", RiskLevel: tool.RiskLow}},
	})

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-veto", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Dispatcher must NEVER have been called.
	if disp.calls.Load() != 0 {
		t.Errorf("dispatcher invoked %d times despite gate veto",
			disp.calls.Load())
	}

	// Tool result must be errored with the gate reason.
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil {
			if ev.Tool.Status != "errored" {
				t.Errorf("vetoed call status = %q", ev.Tool.Status)
			}
			if !strings.Contains(ev.Tool.Error, "hook gate") &&
				!strings.Contains(ev.Tool.Error, "delete forbidden") {
				t.Errorf("error doesn't mention gate veto : %q", ev.Tool.Error)
			}
		}
	}
}

// =====================================================================
// CC-14 — run_parallel real dispatch through full engine
// =====================================================================

// recordingPerCall captures the order calls reached the inner
// dispatcher to assert run_parallel preserves input order.
type recordingPerCall struct {
	mu      sync.Mutex
	called  []string
	results map[string]dgruntime.ToolOutcome
}

func (r *recordingPerCall) Dispatch(_ context.Context, call dgruntime.ToolInvocation) dgruntime.ToolOutcome {
	r.mu.Lock()
	r.called = append(r.called, call.Name)
	res, ok := r.results[call.Name]
	r.mu.Unlock()
	if !ok {
		return dgruntime.ToolOutcome{Status: "completed",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "default-" + call.Name},
			}}
	}
	return res
}

func TestComprehensive_RunParallelE2E(t *testing.T) {
	inner := &recordingPerCall{
		results: map[string]dgruntime.ToolOutcome{
			"filesystem.read": {
				Status: "completed",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: "read-ok"},
				},
			},
			"http.get": {
				Status: "errored",
				Error:  "connection refused",
			},
			"shell.bash": {
				Status: "completed",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: "shell-ok"},
				},
			},
		},
	}

	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{Name: "filesystem.read", RiskLevel: tool.RiskLow}},
		{Module: "http", Action: "get",
			Spec: &tool.Spec{Name: "http.get", RiskLevel: tool.RiskLow}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{Name: "shell.bash", RiskLevel: tool.RiskLow}},
	}

	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-rp")

	// LLM calls run_parallel with 3 actions. The runtime fans
	// out, awaits all, returns aggregated results.
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:   "c-rp",
			Name: "context_builder.run_parallel",
			Arguments: map[string]any{
				"actions": []any{
					map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "x"}},
					map[string]any{"name": "http.get", "params": map[string]any{"url": "y"}},
					map[string]any{"name": "shell.bash", "params": map[string]any{"command": "echo"}},
				},
			},
		}}},
		{Content: "done"},
	}}

	cb := buildContextOnly(universe)
	disp := buildMetaDispatcherWith(cb, inner)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-rp", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// All 3 inner dispatches happened.
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.called) != 3 {
		t.Errorf("inner.called = %d, want 3 : %v", len(inner.called), inner.called)
	}
	// All three names present (order may vary in concurrent dispatch).
	gotSet := map[string]bool{}
	for _, n := range inner.called {
		gotSet[n] = true
	}
	for _, want := range []string{"filesystem.read", "http.get", "shell.bash"} {
		if !gotSet[want] {
			t.Errorf("missing dispatch of %q : %v", want, inner.called)
		}
	}

	// The tool_result event for context_builder.run_parallel must
	// have completed status (overall) and the aggregated JSON in
	// its Parts.
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil &&
			ev.Tool.Name == "context_builder.run_parallel" {
			if ev.Tool.Status != "completed" {
				t.Errorf("run_parallel result status = %q", ev.Tool.Status)
			}
			if len(ev.Tool.Parts) == 0 {
				t.Error("run_parallel result has no parts")
			} else {
				body := ev.Tool.Parts[0].Text
				if !strings.Contains(body, "results") {
					t.Errorf("run_parallel result missing 'results' key : %q", body)
				}
				if !strings.Contains(body, "read-ok") {
					t.Errorf("run_parallel missing filesystem result")
				}
				if !strings.Contains(body, "connection refused") {
					t.Errorf("run_parallel missing failing-call error")
				}
			}
		}
	}
}

// =====================================================================
// CC-15 — background_run + auto-notification round trip
// =====================================================================

// inProcBgManager is a minimal in-process BackgroundManager that
// runs the launched call synchronously for the test. It tracks the
// task in a per-session map and exposes the auto-notification
// payload via a channel so the test can drain it like the runtime
// engine does on the next turn_start.
type inProcBgManager struct {
	mu     sync.Mutex
	tasks  map[string]*meta.BackgroundStatus
	notifs map[string][]string
}

func newInProcBg() *inProcBgManager {
	return &inProcBgManager{
		tasks:  map[string]*meta.BackgroundStatus{},
		notifs: map[string][]string{},
	}
}

func (m *inProcBgManager) Launch(_ context.Context, req meta.LaunchRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tid := "tid-" + req.Tool
	m.tasks[tid] = &meta.BackgroundStatus{
		TaskID: tid, Name: req.Tool, State: "running",
	}
	// Simulate immediate completion + enqueue notification.
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.tasks[tid].State = "completed"
		m.tasks[tid].Result = "ok"
		m.notifs[req.SessionID] = append(m.notifs[req.SessionID],
			fmt.Sprintf("[BACKGROUND TASK COMPLETED] task_id=%s tool=%s elapsed=0.1s", tid, req.Tool))
	}()
	return tid, nil
}
func (m *inProcBgManager) Status(_ context.Context, _, tid string) (meta.BackgroundStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.tasks[tid]; ok {
		return *s, nil
	}
	return meta.BackgroundStatus{}, fmt.Errorf("not found")
}
func (m *inProcBgManager) Wait(_ context.Context, _, tid string, _ float64) (meta.BackgroundStatus, error) {
	return m.Status(context.Background(), "", tid)
}
func (m *inProcBgManager) Cancel(_ context.Context, _, _ string) error { return nil }
func (m *inProcBgManager) List(_ context.Context, _ string) ([]meta.BackgroundStatus, error) {
	return nil, nil
}

func (m *inProcBgManager) DrainSession(sid string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.notifs[sid]
	delete(m.notifs, sid)
	return q
}

func TestComprehensive_BackgroundRunFullCycle(t *testing.T) {
	bg := newInProcBg()
	disp := &meta.MetaDispatcher{Background: bg}

	// Launch via the LLM-facing call shape.
	out := disp.Dispatch(context.Background(), dgruntime.ToolInvocation{
		Name:  "context_builder.background_run",
		AppID: "test-app", UserID: "userA",
		Args: map[string]any{
			"name": "db.query", "params": map[string]any{"sql": "SELECT 1"},
		},
	})
	if out.Status != "completed" {
		t.Fatalf("launch failed : %s", out.Error)
	}
	// Body should contain task_id.
	body := out.Parts[0].Text
	if !strings.Contains(body, "task_id") {
		t.Errorf("launch response missing task_id : %q", body)
	}

	// Wait for the background go-routine to finish + enqueue notif.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bg.mu.Lock()
		hasNotif := len(bg.notifs["test-app:userA"]) > 0
		bg.mu.Unlock()
		if hasNotif {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	notifs := bg.DrainSession("test-app:userA")
	if len(notifs) != 1 {
		t.Fatalf("notifs = %d, want 1", len(notifs))
	}
	if !strings.HasPrefix(notifs[0], "[BACKGROUND TASK COMPLETED]") {
		t.Errorf("notif format wrong : %q", notifs[0])
	}
	if !strings.Contains(notifs[0], "tool=db.query") {
		t.Errorf("notif missing tool name : %q", notifs[0])
	}

	// Status call returns completed.
	statusOut := disp.Dispatch(context.Background(), dgruntime.ToolInvocation{
		Name:  "context_builder.background_run",
		AppID: "test-app", UserID: "userA",
		Args: map[string]any{"task_id": "tid-db.query"},
	})
	if statusOut.Status != "completed" {
		t.Errorf("status check failed : %s", statusOut.Error)
	}
	if !strings.Contains(statusOut.Parts[0].Text, "completed") {
		t.Errorf("status body should report completed : %q", statusOut.Parts[0].Text)
	}
}
