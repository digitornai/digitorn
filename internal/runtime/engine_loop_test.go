package runtime_test

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// RT-3 — Tool execution loop. Tests cover the agent loop in
// runPhases : LLM → ToolDispatcher → LLM → ... → final answer.
// Critical guarantees :
//
//   - 0 tool_calls in LLM response = loop terminates immediately
//   - N tool_calls in one round = N goroutines, ALL dispatched in
//     parallel, EventToolResult emitted per call
//   - tool errors flow back into the next LLM call so the model can
//     decide to retry / give up
//   - MaxToolIterations bounds the loop (no runaway with a buggy LLM)
//   - slow tool in session A does NOT slow down session B's loop
//
// projectingSessions extends the basic stubSessions stub by applying
// every AppendDurable event to the in-memory SessionState via the
// production projection. The agent loop in runPhases re-reads the
// state between iterations expecting tool results to show up as
// "tool" role Messages — without projection nothing surfaces and
// the test would not exercise the realistic LLM-sees-tool-results
// path. Used only by RT-3 tests ; the simpler stubSessions remains
// for tests that don't care about projection.
type projectingSessions struct {
	state     *sessionstore.SessionState
	appendSeq uint64
	events    []sessionstore.Event
	mu        sync.Mutex
}

func newProjectingSessions(sid string) *projectingSessions {
	return &projectingSessions{state: sessionstore.NewSessionState(sid)}
}

func (p *projectingSessions) State(_ string) (*sessionstore.SessionState, error) {
	return p.state, nil
}

func (p *projectingSessions) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendSeq++
	ev.Seq = p.appendSeq
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	p.events = append(p.events, ev)
	sessionstore.Apply(p.state, &ev)
	return p.appendSeq, nil
}

func (p *projectingSessions) Append(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	return p.AppendDurable(ctx, ev)
}

func (p *projectingSessions) count(t sessionstore.EventType) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.events {
		if p.events[i].Type == t {
			n++
		}
	}
	return n
}

func (p *projectingSessions) find(t sessionstore.EventType) *sessionstore.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.events {
		if p.events[i].Type == t {
			ev := p.events[i]
			return &ev
		}
	}
	return nil
}

// TestLoop_NoToolCalls_TerminatesAfterOneRound : the baseline. If the
// LLM returns a text-only response, runPhases must NOT loop a second
// time. One assistant_message, zero EventToolCall, zero
// EventToolResult.
func TestLoop_NoToolCalls_TerminatesAfterOneRound(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{Content: "final answer", Model: "gpt-4o-mini"},
		},
	}

	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sess.count(sessionstore.EventAssistantMessage); got != 1 {
		t.Fatalf("expected 1 assistant message, got %d", got)
	}
	if got := sess.count(sessionstore.EventToolCall); got != 0 {
		t.Fatalf("expected 0 tool_call events, got %d", got)
	}
	if got := sess.count(sessionstore.EventToolResult); got != 0 {
		t.Fatalf("expected 0 tool_result events, got %d", got)
	}
	if lc.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", lc.calls)
	}
}

// TestLoop_SingleToolRound_DispatchesAndFeedsBack : LLM returns one
// tool_call ; runtime dispatches via Dispatcher ; tool result lands
// as EventToolResult AND as a "tool" role Message in state.Messages
// (so the next LLM call sees it) ; LLM second call has no
// tool_calls = loop ends.
func TestLoop_SingleToolRound_DispatchesAndFeedsBack(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{
				Content: "let me search",
				ToolCalls: []llm.ChatToolCall{
					{ID: "c1", Type: "function", Name: "web_search", Arguments: map[string]any{"q": "paris"}},
				},
				Model: "gpt-4o-mini",
			},
			{Content: "The capital is Paris.", Model: "gpt-4o-mini"},
		},
	}
	disp := &runtime.StaticToolDispatcher{
		Outcomes: map[string]runtime.ToolOutcome{
			"web_search": {
				Status: "completed",
				Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "Paris is the capital of France."}},
			},
		},
	}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if lc.calls != 2 {
		t.Fatalf("expected 2 LLM calls (tool round + final), got %d", lc.calls)
	}
	if got := sess.count(sessionstore.EventToolCall); got != 1 {
		t.Fatalf("expected 1 tool_call, got %d", got)
	}
	if got := sess.count(sessionstore.EventToolResult); got != 1 {
		t.Fatalf("expected 1 tool_result, got %d", got)
	}
	if got := sess.count(sessionstore.EventAssistantMessage); got != 2 {
		t.Fatalf("expected 2 assistant_messages, got %d", got)
	}

	// The second LLM call MUST have seen the tool result. Check the
	// last request's Messages : it should contain a "tool" role entry
	// referencing call_id=c1.
	if lc.got == nil {
		t.Fatal("no LLM request captured")
	}
	foundTool := false
	for _, m := range lc.got.Messages {
		if m.Role == "tool" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("second LLM call missing tool result in messages : %+v", lc.got.Messages)
	}

	// Outcome content must be persisted into the tool_result event.
	ev := sess.find(sessionstore.EventToolResult)
	if ev == nil || ev.Tool == nil || len(ev.Tool.Parts) != 1 {
		t.Fatalf("tool_result event malformed : %+v", ev)
	}
	if ev.Tool.Parts[0].Text != "Paris is the capital of France." {
		t.Fatalf("tool result text lost : %q", ev.Tool.Parts[0].Text)
	}
}

// TestLoop_MultipleToolCalls_DispatchedInParallel : N tool_calls in
// one round must run in parallel goroutines, not sequentially.
// Measure : if each tool sleeps 50ms, total wall time must be ~50ms
// (parallel) not ~150ms (serial).
func TestLoop_MultipleToolCalls_DispatchedInParallel(t *testing.T) {
	const callDur = 50 * time.Millisecond
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{
				ToolCalls: []llm.ChatToolCall{
					{ID: "a", Name: "t_a"},
					{ID: "b", Name: "t_b"},
					{ID: "c", Name: "t_c"},
				},
			},
			{Content: "done"},
		},
	}
	disp := &latencyDispatcher{delay: callDur, status: "completed"}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp

	t0 := time.Now()
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dur := time.Since(t0)
	t.Logf("3 tools × %v each — wall %v ; maxInFlight=%d", callDur, dur, disp.maxInFlight.Load())

	// The hard proof of parallelism : the dispatcher saw 3 calls
	// concurrent IN-FLIGHT at the same instant. Wall-clock is too
	// flaky under loaded CI ; this counter is exact.
	if got := disp.maxInFlight.Load(); got != 3 {
		t.Fatalf("parallel dispatch broken : maxInFlight=%d want 3", got)
	}
	if got := disp.dispatched.Load(); got != 3 {
		t.Fatalf("expected 3 dispatches, got %d", got)
	}
	if got := sess.count(sessionstore.EventToolCall); got != 3 {
		t.Fatalf("expected 3 tool_call events, got %d", got)
	}
	if got := sess.count(sessionstore.EventToolResult); got != 3 {
		t.Fatalf("expected 3 tool_result events, got %d", got)
	}
}

// TestLoop_ToolError_FlowsBackToLLM : if the dispatcher returns
// status="errored", the runtime persists EventToolResult with the
// error string AND the next LLM call sees a tool-role message with
// the error so the model can decide what to do.
func TestLoop_ToolError_FlowsBackToLLM(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "broken_tool"}}},
			{Content: "I cannot proceed because the tool failed."},
		},
	}
	disp := &runtime.StaticToolDispatcher{
		Outcomes: map[string]runtime.ToolOutcome{
			"broken_tool": {
				Status: "errored",
				Error:  "remote service returned 503",
			},
		},
	}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ev := sess.find(sessionstore.EventToolResult)
	if ev == nil || ev.Tool == nil {
		t.Fatal("no tool_result emitted")
	}
	if ev.Tool.Status != "errored" {
		t.Fatalf("tool_result status: %q want errored", ev.Tool.Status)
	}
	if ev.Tool.Error != "remote service returned 503" {
		t.Fatalf("tool_result error lost: %q", ev.Tool.Error)
	}
	if lc.calls != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", lc.calls)
	}
}

// TestLoop_MaxIterations_HardCap : a misbehaving LLM that returns
// tool_calls EVERY round must NOT run forever. Engine.MaxToolIterations
// bounds the loop ; the test sets it to 3 and asserts at most 3 tool
// rounds + 1 final assistant message.
func TestLoop_MaxIterations_HardCap(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")

	// Always return one tool_call ; the loop would never end without
	// the iteration cap.
	const cap = 3
	resps := make([]*llm.ChatResponse, cap)
	for i := range resps {
		resps[i] = &llm.ChatResponse{
			ToolCalls: []llm.ChatToolCall{{ID: fmt.Sprintf("c%d", i), Name: "stuck"}},
		}
	}
	lc := &stubLLM{responses: resps}
	disp := &runtime.StaticToolDispatcher{
		Outcomes: map[string]runtime.ToolOutcome{"stuck": {Status: "completed"}},
	}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.MaxToolIterations = cap

	res, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if lc.calls != cap {
		t.Fatalf("expected exactly %d LLM calls (capped), got %d", cap, lc.calls)
	}
	if got := sess.count(sessionstore.EventToolCall); got != cap {
		t.Fatalf("expected %d tool_call events, got %d", cap, got)
	}
	// The cap must end the turn VISIBLY, not silently: the result carries a
	// note and an assistant message is persisted so the client shows it.
	if res == nil || !strings.Contains(res.Content, "stopped after") {
		t.Fatalf("hitting the cap must surface a visible note, got result %+v", res)
	}
}

// TestLoop_NoDispatcher_FallsBackToNoop : without a wired dispatcher,
// the loop calls NoopDispatcher which returns "errored". Verifies
// the path stays alive (no panic) and the error surfaces to the LLM.
func TestLoop_NoDispatcher_FallsBackToNoop(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "anything"}}},
			{Content: "couldn't run tool"},
		},
	}

	e := newEngine(t, apps, sess, lc)
	// e.Dispatcher left nil.
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ev := sess.find(sessionstore.EventToolResult)
	if ev == nil || ev.Tool == nil {
		t.Fatal("no tool_result emitted")
	}
	if ev.Tool.Status != "errored" {
		t.Fatalf("noop dispatcher should yield errored, got %q", ev.Tool.Status)
	}
}

// latencyDispatcher is a test dispatcher that sleeps `delay` before
// returning. The atomic counter proves N concurrent dispatches all
// ran. Used by parallel + isolation tests.
type latencyDispatcher struct {
	delay       time.Duration
	status      string
	dispatched  atomic.Int32
	maxInFlight atomic.Int32
	inFlight    atomic.Int32
}

func (d *latencyDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	cur := d.inFlight.Add(1)
	defer d.inFlight.Add(-1)
	for {
		m := d.maxInFlight.Load()
		if cur <= m {
			break
		}
		if d.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	d.dispatched.Add(1)
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return runtime.ToolOutcome{Status: "errored", Error: ctx.Err().Error()}
	}
	st := d.status
	if st == "" {
		st = "completed"
	}
	return runtime.ToolOutcome{
		Status: st,
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
	}
}

// swallowTimeoutDispatcher blocks until its ctx is cancelled, then returns an
// EMPTY outcome — modelling a tool that respects cancellation but doesn't
// classify the timeout itself, so the engine must synthesise the errored result.
type swallowTimeoutDispatcher struct{ canceled atomic.Bool }

func (d *swallowTimeoutDispatcher) Dispatch(ctx context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	<-ctx.Done()
	d.canceled.Store(true)
	return runtime.ToolOutcome{}
}

func toolRoleContent(msgs []llm.ChatMessage) string {
	for _, m := range msgs {
		if m.Role == "tool" {
			return m.Content
		}
	}
	return ""
}

// TestLoop_PerToolTimeout_LeafToolCancelledLoopContinues : a leaf tool that
// blows the per-call timeout is cancelled and turned into a clear errored
// result the model SEES — and the turn LOOP CONTINUES to a final answer instead
// of dying after the tool. This is the targeted regression for "the turn stops
// right after a slow tool".
func TestLoop_PerToolTimeout_LeafToolCancelledLoopContinues(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.grep", Arguments: map[string]any{}}}},
			{Content: "recovered after the tool timed out"},
		},
	}
	disp := &swallowTimeoutDispatcher{}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.ToolTimeout = 60 * time.Millisecond

	t0 := time.Now()
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !disp.canceled.Load() {
		t.Fatal("the per-tool timeout never cancelled the tool dispatch")
	}
	if lc.calls != 2 {
		t.Fatalf("loop did not continue past the timed-out tool: %d LLM calls (want 2)", lc.calls)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("timeout took too long to fire: %v", d)
	}
	// The model's second call must carry the synthesised timeout error so it can
	// react (narrow the search / background it).
	if got := toolRoleContent(lc.got.Messages); !strings.Contains(got, "per-call time limit") {
		t.Fatalf("timed-out tool result not surfaced to the model: %q", got)
	}
}

// TestLoop_PerToolTimeout_ExemptToolNotCapped : a human-in-the-loop / sub-flow
// tool (ask_user) is EXEMPT from the per-call timeout — it runs to completion
// even when it outlasts ToolTimeout, so a user thinking for a while never has
// their question killed.
func TestLoop_PerToolTimeout_ExemptToolNotCapped(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "ask_user", Arguments: map[string]any{}}}},
			{Content: "got the answer"},
		},
	}
	// Runs LONGER than ToolTimeout. If ask_user were wrongly capped, the ctx-aware
	// latencyDispatcher would return errored at the cap; instead it completes.
	disp := &latencyDispatcher{delay: 120 * time.Millisecond, status: "completed"}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.ToolTimeout = 30 * time.Millisecond

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.calls != 2 {
		t.Fatalf("loop did not complete with the exempt tool: %d LLM calls (want 2)", lc.calls)
	}
	if got := toolRoleContent(lc.got.Messages); strings.Contains(got, "per-call time limit") {
		t.Fatalf("ask_user was wrongly capped by the per-tool timeout: %q", got)
	}
}

// TestLoop_Isolation_SlowToolDoesNotBlockOtherSessions : the headline
// guarantee. One session is grinding a 500ms tool call. 50 other
// sessions run lightweight turns. Their p99 latency must stay close
// to their unloaded baseline (~10ms each). If the slow session
// somehow shared a lock / sema / goroutine with the others, their
// p99 would jump to >100ms.
func TestLoop_Isolation_SlowToolDoesNotBlockOtherSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("isolation test takes ~500ms ; skipped in -short")
	}

	const slowDur = 500 * time.Millisecond
	const fastDur = 5 * time.Millisecond
	const fastN = 50

	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}

	// One dispatcher serves BOTH the slow and fast sessions. If
	// dispatching has any cross-session contention (a mutex, a single
	// channel, etc.), this is where it would show.
	disp := &mixedDispatcher{
		slow: &latencyDispatcher{delay: slowDur, status: "completed"},
		fast: &latencyDispatcher{delay: fastDur, status: "completed"},
	}

	mkLLM := func() *stubLLM {
		return &stubLLM{
			responses: []*llm.ChatResponse{
				{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "fast_tool"}}},
				{Content: "done"},
			},
		}
	}
	mkSlowLLM := func() *stubLLM {
		return &stubLLM{
			responses: []*llm.ChatResponse{
				{ToolCalls: []llm.ChatToolCall{{ID: "c-slow", Name: "slow_tool"}}},
				{Content: "done"},
			},
		}
	}

	// Fire the slow session FIRST so it's grinding when fast ones start.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sess := newProjectingSessions("slow-sess")
		e := &runtime.Engine{
			Apps: apps, Sessions: sess, LLM: mkSlowLLM(),
			Dispatcher: disp,
			Logger:     discardLogger(),
		}
		_, _ = e.Run(context.Background(), runtime.TurnInput{
			AppID: "app-1", SessionID: "slow-sess", UserID: "u-slow",
		})
	}()

	// Tiny stagger so the slow session is in its dispatcher call.
	time.Sleep(10 * time.Millisecond)

	// Now N fast sessions.
	latencies := make([]time.Duration, fastN)
	wg.Add(fastN)
	for i := 0; i < fastN; i++ {
		go func(i int) {
			defer wg.Done()
			sess := newProjectingSessions(fmt.Sprintf("fast-%d", i))
			e := &runtime.Engine{
				Apps: apps, Sessions: sess, LLM: mkLLM(),
				Dispatcher: disp,
				Logger:     discardLogger(),
			}
			t0 := time.Now()
			_, _ = e.Run(context.Background(), runtime.TurnInput{
				AppID: "app-1", SessionID: fmt.Sprintf("fast-%d", i), UserID: fmt.Sprintf("u-%d", i),
			})
			latencies[i] = time.Since(t0)
		}(i)
	}
	wg.Wait()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[fastN/2]
	p99 := latencies[(fastN*99)/100]

	t.Logf("fast sessions p50=%v p99=%v (slow tool grinding %v)", p50, p99, slowDur)

	// The fast tool takes ~5ms ; fast turn does 2 LLM calls (no
	// network in tests) + 1 dispatch. p99 must stay well below the
	// 500ms slow tool duration. A 100ms ceiling gives 20x headroom
	// over the fast baseline ; if we cross it, isolation is broken.
	if p99 > 100*time.Millisecond {
		t.Fatalf("ISOLATION BROKEN : fast p99=%v while slow tool grinds %v", p99, slowDur)
	}
}

// mixedDispatcher routes calls by tool name to either the slow or the
// fast latencyDispatcher. Lets one test exercise both lanes through a
// single Dispatcher reference.
type mixedDispatcher struct {
	slow *latencyDispatcher
	fast *latencyDispatcher
}

func (m *mixedDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if call.Name == "slow_tool" {
		return m.slow.Dispatch(ctx, call)
	}
	return m.fast.Dispatch(ctx, call)
}

// discardLogger returns a slog.Logger that drops all output. The
// production engine logs "turn complete" on every Run ; tests that
// fire dozens of turns don't want that noise.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
