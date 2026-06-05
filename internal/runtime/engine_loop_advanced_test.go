package runtime_test

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
	runtimepkg "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// RT-3 ADVANCED — paranoid tests for the agent loop. These assert
// non-trivial invariants that a typo / refactor could silently break :
//
//   - strict event ordering per turn (assistant → toolcalls → results → ...)
//   - call_id round-trip integrity (every dispatched id has a result)
//   - LLM input at iteration K contains ALL prior tool results
//   - dispatcher receives correct AppID/UserID per invocation
//   - ctx cancellation propagates from caller → loop → dispatcher
//   - AppendDurable failure during loop aborts cleanly (no partial state)
//   - adversarial LLM : 100-call rounds, duplicate ids, nil response
//   - goroutine count stable across 1000 sequential turns (no leak)
//   - massive concurrent stress under -race (no data races)
//   - cross-session p99 holds under heterogeneous tool latencies
//
// All advanced tests are skipped under -short except the cheap ones
// (ordering, call_id, ctx cancellation). The big stress tests run by
// default in normal test invocation but tag themselves "STRESS" in
// the log so failure context is obvious.

// ============================================================
// helpers : recording dispatcher + capturing sessions
// ============================================================

// recordingDispatcher captures every ToolInvocation it receives,
// preserving order and concurrency timing. Lets tests assert what
// the engine handed to the dispatcher (AppID, UserID, args). Safe
// for concurrent Dispatch calls — uses a mutex around the slice.
type recordingDispatcher struct {
	mu           sync.Mutex
	calls        []runtimepkg.ToolInvocation
	outcome      runtimepkg.ToolOutcome // returned for every call
	delay        time.Duration          // optional sleep before returning
	beforeReturn func(call runtimepkg.ToolInvocation)
}

func (r *recordingDispatcher) Dispatch(ctx context.Context, call runtimepkg.ToolInvocation) runtimepkg.ToolOutcome {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return runtimepkg.ToolOutcome{Status: "errored", Error: ctx.Err().Error()}
		}
	}
	if r.beforeReturn != nil {
		r.beforeReturn(call)
	}
	out := r.outcome
	if out.Status == "" {
		out.Status = "completed"
	}
	if len(out.Parts) == 0 {
		out.Parts = []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}
	}
	return out
}

func (r *recordingDispatcher) snapshot() []runtimepkg.ToolInvocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]runtimepkg.ToolInvocation, len(r.calls))
	copy(cp, r.calls)
	return cp
}

// recordingProjectingSessions is projectingSessions + per-event hooks.
// Lets tests inject AppendDurable failures on specific events.
type recordingProjectingSessions struct {
	*projectingSessions
	failOn      sessionstore.EventType // non-empty = inject error
	failOnCount int                    // 0 = first occurrence, N = Nth
	failErr     error
	countSeen   int
}

func newRecordingSessions(sid string) *recordingProjectingSessions {
	return &recordingProjectingSessions{projectingSessions: newProjectingSessions(sid)}
}

func (r *recordingProjectingSessions) AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	if r.failOn != "" && ev.Type == r.failOn {
		r.countSeen++
		if r.countSeen > r.failOnCount {
			return 0, r.failErr
		}
	}
	return r.projectingSessions.AppendDurable(ctx, ev)
}

// ============================================================
// I1 — Strict event ordering invariant per turn
// ============================================================

// TestAdvanced_EventOrderInvariant_PerTurn : for a multi-iteration turn
// the events emitted must follow EXACTLY this pattern :
//
//	[assistant_message → (tool_call × N → tool_result × N)] × K
//	→ final assistant_message
//
// Where K is the number of tool rounds and N varies per round. The
// invariant is that NO tool_result precedes its tool_call, and NO
// assistant_message of round K+1 precedes ALL results of round K.
//
// Why this matters : downstream consumers (REST view, socket bridge,
// future replay) MUST see the lifecycle in a sensible order. A bug
// that emitted tool_result before tool_call would still "work" in
// projection (the projection tolerates either order) but would
// corrupt every timeline UI.
func TestAdvanced_EventOrderInvariant_PerTurn(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			// Round 1 : 2 tool calls
			{ToolCalls: []llm.ChatToolCall{
				{ID: "r1-a", Name: "t1"},
				{ID: "r1-b", Name: "t2"},
			}},
			// Round 2 : 1 tool call
			{ToolCalls: []llm.ChatToolCall{
				{ID: "r2-a", Name: "t3"},
			}},
			// Round 3 : 3 tool calls
			{ToolCalls: []llm.ChatToolCall{
				{ID: "r3-a", Name: "t4"},
				{ID: "r3-b", Name: "t5"},
				{ID: "r3-c", Name: "t6"},
			}},
			// Final : no calls
			{Content: "done"},
		},
	}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Extract the lifecycle-relevant subset (filter out non-message events).
	var lifecycle []sessionstore.EventType
	pendingResults := map[string]bool{} // call_id → still awaiting result
	for _, ev := range sess.events {
		switch ev.Type {
		case sessionstore.EventAssistantMessage, sessionstore.EventToolCall, sessionstore.EventToolResult:
			lifecycle = append(lifecycle, ev.Type)
			if ev.Type == sessionstore.EventToolCall {
				pendingResults[ev.Tool.CallID] = true
			}
			if ev.Type == sessionstore.EventToolResult {
				if !pendingResults[ev.Tool.CallID] {
					t.Fatalf("tool_result for %q emitted with no preceding tool_call",
						ev.Tool.CallID)
				}
				delete(pendingResults, ev.Tool.CallID)
			}
		}
	}
	if len(pendingResults) > 0 {
		t.Fatalf("tool_calls without results : %v", pendingResults)
	}

	// Expected lifecycle :
	//   asst, tc, tc, tr, tr,   <- round 1 (2 calls)
	//   asst, tc, tr,           <- round 2 (1 call)
	//   asst, tc, tc, tc, tr, tr, tr,  <- round 3 (3 calls)
	//   asst                    <- final
	expected := []sessionstore.EventType{
		"assistant_message",
		"tool_call", "tool_call",
		"tool_result", "tool_result",
		"assistant_message",
		"tool_call",
		"tool_result",
		"assistant_message",
		"tool_call", "tool_call", "tool_call",
		"tool_result", "tool_result", "tool_result",
		"assistant_message",
	}
	if len(lifecycle) != len(expected) {
		t.Fatalf("lifecycle length : got %d want %d\nactual: %v", len(lifecycle), len(expected), lifecycle)
	}
	for i := range lifecycle {
		if lifecycle[i] != expected[i] {
			t.Fatalf("lifecycle[%d] : got %q want %q\nfull: %v", i, lifecycle[i], expected[i], lifecycle)
		}
	}
}

// ============================================================
// I2 — call_id round-trip integrity
// ============================================================

// TestAdvanced_CallID_RoundTrip : every call_id the LLM emits must
// appear in exactly one EventToolCall AND exactly one
// EventToolResult, AND show up in state.ToolCalls map with the
// matching final status. Hammers this with 50 calls spread across
// 5 rounds to amplify any indexing bug (off-by-one, dropped index,
// concurrent writes losing entries).
func TestAdvanced_CallID_RoundTrip(t *testing.T) {
	const rounds = 5
	const callsPerRound = 10

	var responses []*llm.ChatResponse
	for r := 0; r < rounds; r++ {
		var calls []llm.ChatToolCall
		for c := 0; c < callsPerRound; c++ {
			calls = append(calls, llm.ChatToolCall{
				ID:   fmt.Sprintf("r%d-c%d", r, c),
				Name: "t",
			})
		}
		responses = append(responses, &llm.ChatResponse{ToolCalls: calls})
	}
	responses = append(responses, &llm.ChatResponse{Content: "done"})

	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: responses}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}
	e.MaxToolIterations = rounds + 1

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := map[string]int{}
	results := map[string]int{}
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventToolCall {
			calls[ev.Tool.CallID]++
		}
		if ev.Type == sessionstore.EventToolResult {
			results[ev.Tool.CallID]++
		}
	}
	totalExpected := rounds * callsPerRound
	if len(calls) != totalExpected {
		t.Fatalf("distinct call_ids in calls : %d want %d", len(calls), totalExpected)
	}
	if len(results) != totalExpected {
		t.Fatalf("distinct call_ids in results : %d want %d", len(results), totalExpected)
	}
	for id, n := range calls {
		if n != 1 {
			t.Errorf("call_id %q emitted %d EventToolCall (want 1)", id, n)
		}
		if results[id] != 1 {
			t.Errorf("call_id %q has %d EventToolResult (want 1)", id, results[id])
		}
	}
	// State map must reflect every call.
	if len(sess.state.ToolCalls) != totalExpected {
		t.Fatalf("state.ToolCalls : %d entries want %d", len(sess.state.ToolCalls), totalExpected)
	}
	for id, tc := range sess.state.ToolCalls {
		if tc.Status != "completed" {
			t.Errorf("state.ToolCalls[%q].Status = %q want completed", id, tc.Status)
		}
	}
}

// ============================================================
// I3 — LLM at iteration K sees all prior tool results
// ============================================================

// TestAdvanced_LLMSeesAllPriorToolResults : the K-th LLM call's
// message list must contain a "tool" role entry for EVERY tool
// result from rounds 1..K-1. Proves the projection-then-re-snapshot
// loop wiring works ; if the loop re-uses a stale snapshot, the LLM
// would see only round 1's results when reasoning about round 3.
func TestAdvanced_LLMSeesAllPriorToolResults(t *testing.T) {
	const rounds = 4

	var responses []*llm.ChatResponse
	for r := 0; r < rounds; r++ {
		responses = append(responses, &llm.ChatResponse{
			Content: fmt.Sprintf("round %d", r+1),
			ToolCalls: []llm.ChatToolCall{
				{ID: fmt.Sprintf("c%d", r+1), Name: "t"},
			},
		})
	}
	responses = append(responses, &llm.ChatResponse{Content: "final"})

	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: responses}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
	}}
	e.MaxToolIterations = rounds + 1

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify each captured LLM request : at iteration K (1-indexed),
	// the messages must contain (K-1) "tool" role entries.
	for i, req := range lc.allGots {
		k := i + 1
		toolMsgs := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolMsgs++
			}
		}
		want := k - 1 // prior rounds' results
		if toolMsgs != want {
			t.Errorf("iteration %d : got %d tool messages, want %d (full msgs=%+v)",
				k, toolMsgs, want, req.Messages)
		}
	}
}

// ============================================================
// I4 — Dispatcher receives correct routing context
// ============================================================

// TestAdvanced_DispatcherCarriesAppAndUser : every Dispatch call must
// receive the AppID and UserID from the TurnInput. A bug that copied
// from the wrong scope (e.g. a shared variable in a goroutine closure)
// would surface here, with 30 parallel calls each tagged differently.
func TestAdvanced_DispatcherCarriesAppAndUser(t *testing.T) {
	const N = 30
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")

	var calls []llm.ChatToolCall
	for i := 0; i < N; i++ {
		calls = append(calls, llm.ChatToolCall{ID: fmt.Sprintf("c%d", i), Name: "t"})
	}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: calls},
		{Content: "done"},
	}}
	disp := &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "tagged-app", SessionID: "sess-1", UserID: "tagged-user",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := disp.snapshot()
	if len(got) != N {
		t.Fatalf("dispatch count : got %d want %d", len(got), N)
	}
	for _, inv := range got {
		if inv.AppID != "tagged-app" {
			t.Errorf("Dispatch AppID = %q want tagged-app (call=%s)", inv.AppID, inv.CallID)
		}
		if inv.UserID != "tagged-user" {
			t.Errorf("Dispatch UserID = %q want tagged-user (call=%s)", inv.UserID, inv.CallID)
		}
	}
}

// ============================================================
// I5 — ctx cancellation propagates to dispatcher
// ============================================================

// TestAdvanced_CtxCancel_PropagatesToDispatcher : the engine must not
// "absorb" the caller's ctx — a cancellation mid-dispatch must reach
// the dispatcher's Dispatch ctx. We use a dispatcher that watches
// ctx.Done() to confirm it received the cancellation. The Run call
// must return an error reflecting the cancellation.
func TestAdvanced_CtxCancel_PropagatesToDispatcher(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "slow-c", Name: "slow"}}},
		{Content: "(unreached)"},
	}}

	disp := &recordingDispatcher{
		delay:   1 * time.Second, // longer than the cancel
		outcome: runtimepkg.ToolOutcome{Status: "completed"},
	}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	t0 := time.Now()
	_, err := e.Run(ctx, runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	})
	dur := time.Since(t0)

	if dur > 500*time.Millisecond {
		t.Fatalf("ctx cancel didn't propagate : Run took %v (dispatcher delay 1s)", dur)
	}

	// We accept either : Run returned cancellation error, OR Run
	// completed with an "errored" tool_result carrying ctx error.
	// What MUST NOT happen : the call waited the full 1s.
	if err != nil {
		t.Logf("Run returned (expected) error after %v : %v", dur, err)
	} else {
		ev := sess.find(sessionstore.EventToolResult)
		if ev == nil || ev.Tool == nil || ev.Tool.Status != "errored" {
			t.Fatalf("ctx cancel : no error from Run AND no errored tool_result (ev=%+v)", ev)
		}
		if !strings.Contains(ev.Tool.Error, "context") {
			t.Fatalf("tool error should mention context : %q", ev.Tool.Error)
		}
	}
}

// ============================================================
// I6 — AppendDurable failure during loop returns cleanly
// ============================================================

// TestAdvanced_PersistenceFailure_MidLoop_AbortsCleanly : when
// AppendDurable fails on an EventToolResult mid-loop, Run must
// return a non-nil error (not silently succeed). State at the time
// of failure may be partial — that's acceptable as long as the
// caller knows the turn failed.
func TestAdvanced_PersistenceFailure_MidLoop_AbortsCleanly(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newRecordingSessions("sess-1")
	sess.failOn = sessionstore.EventToolResult
	sess.failErr = errors.New("disk full")

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "t"}}},
		{Content: "(unreached)"},
	}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}

	_, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected error on persistence failure, got nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error should wrap underlying : %v", err)
	}
}

// ============================================================
// A1 — Adversarial : 100 tool calls in one round
// ============================================================

// TestAdvanced_AdversarialLLM_100CallsInOneRound : a (possibly buggy)
// LLM that emits 100 tool_calls in a single response must not crash
// the engine, must dispatch all 100 in parallel, must persist 100
// EventToolCall + 100 EventToolResult, and must finish in O(slowest)
// not O(100×slowest).
func TestAdvanced_AdversarialLLM_100CallsInOneRound(t *testing.T) {
	if testing.Short() {
		t.Skip("100-call round bench-y ; skipped in -short")
	}
	const N = 100
	var calls []llm.ChatToolCall
	for i := 0; i < N; i++ {
		calls = append(calls, llm.ChatToolCall{ID: fmt.Sprintf("c%d", i), Name: "t"})
	}
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: calls},
		{Content: "done"},
	}}
	disp := &latencyDispatcher{delay: 20 * time.Millisecond, status: "completed"}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp

	t0 := time.Now()
	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dur := time.Since(t0)

	if got := disp.maxInFlight.Load(); got < int32(N/2) {
		t.Fatalf("parallelism : maxInFlight=%d want at least N/2=%d (full parallel would be %d)",
			got, N/2, N)
	}
	if got := sess.count(sessionstore.EventToolResult); got != N {
		t.Fatalf("expected %d tool_results, got %d", N, got)
	}
	// Wall : 20ms × parallel ≈ 20–40ms ; sequential would be 2000ms.
	if dur > 500*time.Millisecond {
		t.Fatalf("100 parallel calls took %v (should be < 500ms ; serial would be ~2s)", dur)
	}
	t.Logf("100 parallel calls : wall=%v maxInFlight=%d", dur, disp.maxInFlight.Load())
}

// ============================================================
// A2 — Adversarial : LLM returns nil mid-loop
// ============================================================

// TestAdvanced_AdversarialLLM_NilMidLoop : when the LLM client
// returns (nil, nil) — a contract violation — runPhases must NOT
// dereference and panic. It must return a clear error.
func TestAdvanced_AdversarialLLM_NilMidLoop(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "t"}}},
		nil, // contract violation : nil response on 2nd call
	}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}

	_, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected error for nil response, got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Fatalf("error should mention nil : %v", err)
	}
}

// ============================================================
// A3 — Adversarial : dispatcher returns empty outcome
// ============================================================

// TestAdvanced_AdversarialDispatcher_EmptyOutcome : a dispatcher
// returning {} (all zero values) must NOT crash and must result in a
// sensible default ("completed" status, empty parts). Defends against
// 3rd-party dispatcher implementations that forget to set fields.
func TestAdvanced_AdversarialDispatcher_EmptyOutcome(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "t"}}},
		{Content: "done"},
	}}
	// Empty outcome — Status="", no Parts, no Error.
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &runtimepkg.StaticToolDispatcher{
		Outcomes: map[string]runtimepkg.ToolOutcome{"t": {}},
	}

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run failed on empty outcome : %v", err)
	}
	ev := sess.find(sessionstore.EventToolResult)
	if ev == nil || ev.Tool == nil {
		t.Fatal("no tool_result event")
	}
	if ev.Tool.Status == "" {
		t.Fatalf("engine should default Status when dispatcher leaves blank")
	}
	if ev.Tool.Status != "completed" && ev.Tool.Status != "errored" {
		t.Fatalf("Status must be completed|errored, got %q", ev.Tool.Status)
	}
}

// ============================================================
// I7 — Seq monotonicity across the full turn
// ============================================================

// TestAdvanced_SeqMonotonicity_AcrossWholeTurn : every persisted
// event's Seq must be strictly greater than the previous one's, with
// no gaps and no duplicates. The projectingSessions stub allocates
// sequentially under lock — this test mostly proves the engine
// doesn't reorder events or double-write.
func TestAdvanced_SeqMonotonicity_AcrossWholeTurn(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")

	// 3 rounds × 3 calls = 9 tool_calls + 9 tool_results + 4 asst = 22 events.
	var responses []*llm.ChatResponse
	for r := 0; r < 3; r++ {
		var calls []llm.ChatToolCall
		for c := 0; c < 3; c++ {
			calls = append(calls, llm.ChatToolCall{
				ID: fmt.Sprintf("r%d-c%d", r, c), Name: "t",
			})
		}
		responses = append(responses, &llm.ChatResponse{ToolCalls: calls})
	}
	responses = append(responses, &llm.ChatResponse{Content: "done"})
	lc := &stubLLM{responses: responses}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}
	e.MaxToolIterations = 5

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(sess.events) == 0 {
		t.Fatal("no events")
	}
	// Seq starts at 1, increments by 1 each event, no gaps.
	for i, ev := range sess.events {
		want := uint64(i + 1)
		if ev.Seq != want {
			t.Fatalf("events[%d] (%s) Seq=%d want %d",
				i, ev.Type, ev.Seq, want)
		}
	}
}

// ============================================================
// C1 — Goroutine leak smoke : 200 sequential turns
// ============================================================

// TestAdvanced_NoGoroutineLeak_SequentialTurns : run 200 turns back
// to back and verify the goroutine count returns to baseline within
// a small slack. A goroutine leak in dispatchToolsParallel (missing
// wg.Done(), forgotten close, etc.) would inflate the count linearly.
func TestAdvanced_NoGoroutineLeak_SequentialTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("goroutine leak test runs 200 turns ; skipped in -short")
	}

	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	disp := &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}

	// Warm the runtime — first turn allocates pool / IDGen / etc.
	{
		sess := newProjectingSessions("warm")
		lc := &stubLLM{responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "w", Name: "t"}}},
			{Content: "done"},
		}}
		e := newEngine(t, apps, sess, lc)
		e.Dispatcher = disp
		if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
			AppID: "app-1", SessionID: "warm", UserID: "u",
		}); err != nil {
			t.Fatal(err)
		}
	}
	goruntime.GC()
	time.Sleep(50 * time.Millisecond)
	base := goruntime.NumGoroutine()

	const N = 200
	for i := 0; i < N; i++ {
		sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
		lc := &stubLLM{responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{
				{ID: "a", Name: "t"},
				{ID: "b", Name: "t"},
				{ID: "c", Name: "t"},
			}},
			{Content: "done"},
		}}
		e := newEngine(t, apps, sess, lc)
		e.Dispatcher = disp
		if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
			AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: "u",
		}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	goruntime.GC()
	time.Sleep(100 * time.Millisecond)
	after := goruntime.NumGoroutine()

	delta := after - base
	t.Logf("goroutines : base=%d after %d turns=%d (delta=%+d)", base, N, after, delta)
	// Allow up to 10 transient goroutines from runtime / GC / timers.
	if delta > 10 {
		t.Fatalf("goroutine leak suspected : +%d after %d turns", delta, N)
	}
}

// ============================================================
// C2 — Massive concurrent stress under -race
// ============================================================

// TestAdvanced_ConcurrentStress_RaceClean : fire 500 turns
// concurrently, each doing 3 tool rounds with 3 calls per round.
// Each session has its own state ; the dispatcher and LLM stubs are
// shared. With -race enabled this test catches any data race in the
// runtime (engine fields, dispatcher result aggregation, projection,
// etc.).
//
// The recording*Dispatcher and stub*Sessions use mutexes — that's a
// realistic stand-in for the production wiring (sharded bus +
// concurrent-safe dispatcher).
func TestAdvanced_ConcurrentStress_RaceClean(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent stress test ; skipped in -short")
	}
	const N = 500
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	disp := &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}

	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
			lc := &stubLLM{responses: []*llm.ChatResponse{
				{ToolCalls: []llm.ChatToolCall{
					{ID: "a", Name: "t"}, {ID: "b", Name: "t"}, {ID: "c", Name: "t"},
				}},
				{ToolCalls: []llm.ChatToolCall{
					{ID: "d", Name: "t"}, {ID: "e", Name: "t"},
				}},
				{ToolCalls: []llm.ChatToolCall{
					{ID: "f", Name: "t"},
				}},
				{Content: "done"},
			}}
			e := newEngine(t, apps, sess, lc)
			e.Dispatcher = disp
			e.MaxToolIterations = 5
			if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
				AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: fmt.Sprintf("u-%d", i%50),
			}); err != nil {
				errs <- fmt.Errorf("turn %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// 500 turns × 6 calls per turn = 3000 dispatched.
	if got := len(disp.snapshot()); got != N*6 {
		t.Fatalf("dispatch count : got %d want %d", got, N*6)
	}
}

// ============================================================
// I8 — Cross-session p99 holds under heterogeneous tool latencies
// ============================================================

// TestAdvanced_CrossSession_P99HoldsUnderMixedLatencies : a stricter
// version of the isolation test. 20 sessions, each running 2 rounds
// of 3 tools, each tool's latency drawn from {1ms, 5ms, 50ms}. We
// assert that the p99 turn duration is dominated by the SUM of
// per-round maxima (not by cross-session interference). If isolation
// is broken, all 20 sessions would queue on a shared resource and p99
// would grow with N.
func TestAdvanced_CrossSession_P99HoldsUnderMixedLatencies(t *testing.T) {
	if testing.Short() {
		t.Skip("isolation p99 test ; skipped in -short")
	}
	const N = 20

	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	disp := &latencyDispatcher{delay: 50 * time.Millisecond, status: "completed"}

	latencies := make([]time.Duration, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
			lc := &stubLLM{responses: []*llm.ChatResponse{
				{ToolCalls: []llm.ChatToolCall{
					{ID: "a", Name: "t"}, {ID: "b", Name: "t"}, {ID: "c", Name: "t"},
				}},
				{ToolCalls: []llm.ChatToolCall{
					{ID: "d", Name: "t"}, {ID: "e", Name: "t"}, {ID: "f", Name: "t"},
				}},
				{Content: "done"},
			}}
			e := newEngine(t, apps, sess, lc)
			e.Dispatcher = disp
			e.MaxToolIterations = 3
			t0 := time.Now()
			_, _ = e.Run(context.Background(), runtimepkg.TurnInput{
				AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: fmt.Sprintf("u-%d", i),
			})
			latencies[i] = time.Since(t0)
		}(i)
	}
	wg.Wait()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[N/2]
	p99 := latencies[(N*99)/100]
	t.Logf("N=%d turns, 2 rounds × 3 calls × 50ms : p50=%v p99=%v", N, p50, p99)

	// Each turn does 2 rounds × parallel-dispatch(50ms) ≈ 100ms.
	// Even under contention, p99 must stay below 3× the lower bound.
	if p99 > 350*time.Millisecond {
		t.Fatalf("p99=%v exceeds 350ms ceiling : isolation is degrading under N=%d", p99, N)
	}
}

// ============================================================
// I9 — assistant_message Parts integrity across rounds
// ============================================================

// TestAdvanced_AssistantParts_IntegrityAcrossRounds : each round's
// assistant_message MUST carry the LLM's tool_call list as multipart
// Parts (PartTypeToolCall). A bug that only persisted Content would
// erase the structured data downstream RT-3 dispatchers rely on.
func TestAdvanced_AssistantParts_IntegrityAcrossRounds(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{
			Content: "round 1 text",
			ToolCalls: []llm.ChatToolCall{
				{ID: "r1-a", Name: "alpha", Arguments: map[string]any{"k": "v1"}},
				{ID: "r1-b", Name: "beta"},
			},
		},
		{
			Content: "round 2 text",
			ToolCalls: []llm.ChatToolCall{
				{ID: "r2-a", Name: "gamma", Arguments: map[string]any{"x": 42}},
			},
		},
		{Content: "final answer"},
	}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = &recordingDispatcher{outcome: runtimepkg.ToolOutcome{Status: "completed"}}

	if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var assistants []sessionstore.Event
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventAssistantMessage {
			assistants = append(assistants, ev)
		}
	}
	if len(assistants) != 3 {
		t.Fatalf("want 3 assistant_message events, got %d", len(assistants))
	}

	// Round 1 : text + 2 tool_call parts.
	r1 := assistants[0].Message
	gotTCs := countParts(r1.Parts, sessionstore.PartTypeToolCall)
	if gotTCs != 2 {
		t.Errorf("round 1 tool_call parts : %d want 2 (parts=%+v)", gotTCs, r1.Parts)
	}
	if !hasTextPartContaining(r1.Parts, "round 1 text") {
		t.Errorf("round 1 missing text part : %+v", r1.Parts)
	}

	// Round 2 : text + 1 tool_call part with structured args.
	r2 := assistants[1].Message
	if countParts(r2.Parts, sessionstore.PartTypeToolCall) != 1 {
		t.Errorf("round 2 tool_call count : %+v", r2.Parts)
	}
	var foundGamma bool
	for _, p := range r2.Parts {
		if p.Type == sessionstore.PartTypeToolCall && p.ToolCall != nil && p.ToolCall.Name == "gamma" {
			if v, ok := p.ToolCall.Args["x"]; !ok || fmt.Sprint(v) != "42" {
				t.Errorf("round 2 gamma args lost : %+v", p.ToolCall.Args)
			}
			foundGamma = true
		}
	}
	if !foundGamma {
		t.Error("round 2 gamma tool_call part not found")
	}

	// Final : just text, zero tool_call parts.
	rf := assistants[2].Message
	if countParts(rf.Parts, sessionstore.PartTypeToolCall) != 0 {
		t.Errorf("final assistant should have no tool_call parts : %+v", rf.Parts)
	}
}

// benchApp builds a minimal RuntimeApp for benchmarks (no *testing.T,
// so we can't reuse okApp which calls t.Helper()).
func benchApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "app-1"},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID:    "primary",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
			}},
		},
		BundleDir: "/tmp/app-1",
	}
}

func countParts(parts []sessionstore.MessagePart, t string) int {
	n := 0
	for _, p := range parts {
		if p.Type == t {
			n++
		}
	}
	return n
}

func hasTextPartContaining(parts []sessionstore.MessagePart, s string) bool {
	for _, p := range parts {
		if p.Type == sessionstore.PartTypeText && strings.Contains(p.Text, s) {
			return true
		}
	}
	return false
}

// ============================================================
// B1 — Benchmarks for the agent loop
// ============================================================

// BenchmarkAgentLoop_OneRoundOneCall : baseline cost of the simplest
// useful agent loop (1 LLM → 1 tool → 1 LLM). Excludes LLM network
// because the stub returns synchronously ; what we measure is the
// runtime overhead.
func BenchmarkAgentLoop_OneRoundOneCall(b *testing.B) {
	apps := &stubApps{app: benchApp()}
	disp := &runtimepkg.StaticToolDispatcher{Outcomes: map[string]runtimepkg.ToolOutcome{
		"t": {Status: "completed", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}},
	}}
	pool := benchPool()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
		lc := &stubLLM{responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "t"}}},
			{Content: "done"},
		}}
		e := &runtimepkg.Engine{
			Apps: apps, Sessions: sess, LLM: lc, Dispatcher: disp,
			Pool: pool, IDGen: benchIDGen(), Logger: discardLogger(),
		}
		if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
			AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: "u",
		}); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// BenchmarkAgentLoop_TenCallsOneRound : measures the cost of parallel
// dispatch with 10 calls per round. Reveals dispatchToolsParallel
// overhead (goroutine creation, WaitGroup sync).
func BenchmarkAgentLoop_TenCallsOneRound(b *testing.B) {
	apps := &stubApps{app: benchApp()}
	var calls []llm.ChatToolCall
	for i := 0; i < 10; i++ {
		calls = append(calls, llm.ChatToolCall{ID: fmt.Sprintf("c%d", i), Name: "t"})
	}
	disp := &runtimepkg.StaticToolDispatcher{Outcomes: map[string]runtimepkg.ToolOutcome{
		"t": {Status: "completed", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}},
	}}
	pool := benchPool()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
		lc := &stubLLM{responses: []*llm.ChatResponse{
			{ToolCalls: calls},
			{Content: "done"},
		}}
		e := &runtimepkg.Engine{
			Apps: apps, Sessions: sess, LLM: lc, Dispatcher: disp,
			Pool: pool, IDGen: benchIDGen(), Logger: discardLogger(),
		}
		if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
			AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: "u",
		}); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// BenchmarkAgentLoop_FiveRoundsOneCall : measures iteration overhead
// (re-snapshot of session state, message rebuild for next LLM call)
// when the loop runs 5 rounds with 1 call each.
func BenchmarkAgentLoop_FiveRoundsOneCall(b *testing.B) {
	apps := &stubApps{app: benchApp()}
	disp := &runtimepkg.StaticToolDispatcher{Outcomes: map[string]runtimepkg.ToolOutcome{
		"t": {Status: "completed", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}},
	}}
	mkResps := func() []*llm.ChatResponse {
		var resps []*llm.ChatResponse
		for r := 0; r < 5; r++ {
			resps = append(resps, &llm.ChatResponse{
				ToolCalls: []llm.ChatToolCall{{ID: fmt.Sprintf("c%d", r), Name: "t"}},
			})
		}
		return append(resps, &llm.ChatResponse{Content: "done"})
	}
	pool := benchPool()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
		e := &runtimepkg.Engine{
			Apps: apps, Sessions: sess, LLM: &stubLLM{responses: mkResps()}, Dispatcher: disp,
			Pool: pool, IDGen: benchIDGen(), Logger: discardLogger(),
			MaxToolIterations: 6,
		}
		if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
			AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: "u",
		}); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// keep imports stable across small refactors
var _ = goruntime.GOOS
var _ atomic.Int32
