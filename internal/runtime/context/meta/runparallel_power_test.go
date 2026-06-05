package meta_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// run_parallel — Go-power guarantees
//
// Proves the fan-out/fan-in is real parallelism, panic-isolated, and
// non-blocking under context cancellation — the things the Python
// asyncio.gather version could not give (single event loop, no true
// multi-core, an unhandled exception could abort the batch).
// =====================================================================

// sleepInner sleeps a fixed duration then completes — used to prove the
// batch runs concurrently (wall time ≈ one action, not N).
type sleepInner struct{ d time.Duration }

func (s *sleepInner) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	time.Sleep(s.d)
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
	}
}

// panicInner panics for one named action, completes for the rest.
type panicInner struct{ boom string }

func (p *panicInner) Dispatch(_ context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	if c.Name == p.boom {
		panic("kaboom in " + c.Name)
	}
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
	}
}

// blockForeverInner blocks on a channel for one named action (ignoring
// ctx on purpose), so the test proves run_parallel ITSELF is the thing
// that returns on cancellation — not a well-behaved sub-tool.
type blockForeverInner struct {
	release chan struct{}
	blockOn string
}

func (b *blockForeverInner) Dispatch(_ context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	if c.Name == b.blockOn {
		<-b.release
	}
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "done"}},
	}
}

// perNameDelayInner sleeps a per-name duration so completion order can be
// made the reverse of input order.
type perNameDelayInner struct{ delays map[string]time.Duration }

func (p *perNameDelayInner) Dispatch(_ context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	time.Sleep(p.delays[c.Name])
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: c.Name}},
	}
}

func powerActions(names ...string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = map[string]any{"name": n, "params": map[string]any{}}
	}
	return out
}

// TestRunParallelPower_TrueConcurrency : N sleeping actions must finish in
// roughly the time of ONE, proving they run on multiple goroutines rather
// than sequentially.
func TestRunParallelPower_TrueConcurrency(t *testing.T) {
	const n = 8
	const d = 50 * time.Millisecond
	disp := &meta.MetaDispatcher{Inner: &sleepInner{d: d}}

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("m.a%d", i)
	}

	start := time.Now()
	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": powerActions(names...)},
	})
	elapsed := time.Since(start)

	body := decodeJSONOutcome(t, out)
	if got := len(body["results"].([]any)); got != n {
		t.Fatalf("results = %d, want %d", got, n)
	}
	// Sequential would be n*d (400ms). Concurrent is ~d (50ms). Allow a
	// generous ceiling so the race detector / loaded CI doesn't flake.
	if ceiling := d * time.Duration(n) / 2; elapsed > ceiling {
		t.Errorf("run_parallel took %v for %d×%v actions (ceiling %v) — not concurrent",
			elapsed, n, d, ceiling)
	}
}

// TestRunParallelPower_PanicIsolation : a panicking action becomes one
// errored result ; its siblings still complete ; the daemon never crashes
// (reaching the assertions at all proves no crash).
func TestRunParallelPower_PanicIsolation(t *testing.T) {
	disp := &meta.MetaDispatcher{Inner: &panicInner{boom: "danger.boom"}}
	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": powerActions("safe.a", "danger.boom", "safe.c")},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	r1 := results[1].(map[string]any)
	if r1["status"] != "errored" {
		t.Errorf("panicking action status = %v, want errored", r1["status"])
	}
	if errStr, _ := r1["error"].(string); !strings.Contains(errStr, "panic") {
		t.Errorf("error should mention panic : %q", errStr)
	}
	for _, i := range []int{0, 2} {
		if results[i].(map[string]any)["status"] != "completed" {
			t.Errorf("sibling[%d] not completed despite a sibling panic", i)
		}
	}
}

// TestRunParallelPower_NonBlockingOnCancel : when the parent ctx is
// cancelled while an action is stuck, run_parallel returns promptly with
// a ctx error for the stragglers instead of hanging on the zombie.
func TestRunParallelPower_NonBlockingOnCancel(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unblock the zombie goroutines at test end
	disp := &meta.MetaDispatcher{Inner: &blockForeverInner{release: release, blockOn: "slow.block"}}

	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan runtime.ToolOutcome, 1)
	go func() {
		resCh <- disp.Dispatch(ctx, runtime.ToolInvocation{
			Name: "context_builder.run_parallel",
			Args: map[string]any{"actions": powerActions("slow.block", "slow.block")},
		})
	}()

	cancel()

	select {
	case out := <-resCh:
		body := decodeJSONOutcome(t, out)
		for i, raw := range body["results"].([]any) {
			r := raw.(map[string]any)
			if r["status"] != "errored" {
				t.Errorf("results[%d] status = %v, want errored (cancelled)", i, r["status"])
			}
			if errStr, _ := r["error"].(string); !strings.Contains(errStr, "cancel") {
				t.Errorf("results[%d] error should mention cancellation : %q", i, errStr)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run_parallel blocked past cancellation — not non-blocking")
	}
}

// TestRunParallelPower_OrderPreservedUnderOutOfOrderCompletion : the
// completion order is the reverse of input order ; the envelope must still
// be input-ordered.
func TestRunParallelPower_OrderPreservedUnderOutOfOrderCompletion(t *testing.T) {
	disp := &meta.MetaDispatcher{Inner: &perNameDelayInner{delays: map[string]time.Duration{
		"x.slow": 60 * time.Millisecond,
		"x.med":  30 * time.Millisecond,
		"x.fast": 1 * time.Millisecond,
	}}}

	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": powerActions("x.slow", "x.med", "x.fast")},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	for i, want := range []string{"x.slow", "x.med", "x.fast"} {
		if got := results[i].(map[string]any)["name"]; got != want {
			t.Errorf("results[%d].name = %v, want %v (input order must hold)", i, got, want)
		}
	}
}

// TestRunParallelPower_RecursionGuardBoundsFanout : a run_parallel nested
// inside a run_parallel is refused before it fans out, so the goroutine
// count can never explode (50^depth). The valid sibling still runs ; the
// deep grandchildren never reach the inner dispatcher.
func TestRunParallelPower_RecursionGuardBoundsFanout(t *testing.T) {
	inner := &echoInner{}
	disp := &meta.MetaDispatcher{Inner: inner}

	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			map[string]any{"name": "safe.a", "params": map[string]any{}},
			map[string]any{"name": "context_builder.run_parallel", "params": map[string]any{
				"actions": []any{
					map[string]any{"name": "deep.x", "params": map[string]any{}},
					map[string]any{"name": "deep.y", "params": map[string]any{}},
				},
			}},
		}},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if results[0].(map[string]any)["status"] != "completed" {
		t.Errorf("safe.a should complete : %v", results[0])
	}
	r1 := results[1].(map[string]any)
	if r1["status"] != "errored" {
		t.Errorf("nested run_parallel should be errored : %v", r1)
	}
	if errStr, _ := r1["error"].(string); !strings.Contains(errStr, "nested") {
		t.Errorf("error should mention the nesting guard : %q", errStr)
	}
	if got := innerNames(inner); containsName(got, "deep.x") || containsName(got, "deep.y") {
		t.Errorf("recursion guard failed : a deep grandchild reached the inner dispatcher : %v", got)
	}
}

// TestRunParallelPower_MalformedActionIsolated : a malformed action becomes
// one errored result ; the rest of the batch still runs (doc : "failures
// in one do not cancel the others").
func TestRunParallelPower_MalformedActionIsolated(t *testing.T) {
	inner := &echoInner{}
	disp := &meta.MetaDispatcher{Inner: inner}

	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			map[string]any{"name": "good.a", "params": map[string]any{}},
			map[string]any{"params": map[string]any{}}, // missing name
			"not an object", // not a map
			map[string]any{"name": "good.b", "params": map[string]any{}},
		}},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (one slot per input action)", len(results))
	}
	wantStatus := []string{"completed", "errored", "errored", "completed"}
	for i, want := range wantStatus {
		if got := results[i].(map[string]any)["status"]; got != want {
			t.Errorf("results[%d] status = %v, want %v", i, got, want)
		}
	}
	got := innerNames(inner)
	if !containsName(got, "good.a") || !containsName(got, "good.b") {
		t.Errorf("valid actions must still run despite malformed siblings : %v", got)
	}
}

// TestRunParallelPower_HeterogeneousToolFamilies : run_parallel is
// tool-family-agnostic. One call fans out a domain tool, an execute_tool
// indirection, a background_run launch, and a use_skill — each is routed
// to the right handler and completes. Proves "any tool works" as a child.
func TestRunParallelPower_HeterogeneousToolFamilies(t *testing.T) {
	inner := &echoInner{}
	bg := &fakeBg{}
	loader := &fakeSkillLoader{entry: meta.SkillEntry{Command: "/commit", Content: "do it"}}
	disp := &meta.MetaDispatcher{Inner: inner, Background: bg, SkillLoader: loader}

	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			// 1. plain domain tool → inner dispatcher
			map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "/a"}},
			// 2. meta indirection : execute_tool wrapping a domain tool → inner
			map[string]any{"name": "context_builder.execute_tool", "params": map[string]any{
				"name": "http.get", "params": map[string]any{"url": "x"},
			}},
			// 3. primitive : background_run launch → background manager
			map[string]any{"name": "context_builder.background_run", "params": map[string]any{
				"name": "database.sql", "params": map[string]any{"q": "SELECT 1"},
			}},
			// 4. primitive : use_skill → skill loader
			map[string]any{"name": "context_builder.use_skill", "params": map[string]any{"command": "/commit"}},
		}},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	for i, raw := range results {
		if got := raw.(map[string]any)["status"]; got != "completed" {
			t.Errorf("results[%d] status = %v, want completed : %v", i, got, raw)
		}
	}
	got := innerNames(inner)
	if !containsName(got, "filesystem.read") {
		t.Error("plain domain tool did not reach the inner dispatcher")
	}
	if !containsName(got, "http.get") {
		t.Error("execute_tool's resolved target did not reach the inner dispatcher")
	}
	if bg.launchCalls != 1 {
		t.Errorf("background_run launches = %d, want 1", bg.launchCalls)
	}
}

// TestRunParallelPower_PanicLoggedWhenLoggerWired : when a logger is wired,
// a recovered sub-tool panic is logged with its stack trace (observability),
// while still being converted to an errored result.
func TestRunParallelPower_PanicLoggedWhenLoggerWired(t *testing.T) {
	var buf bytes.Buffer
	disp := &meta.MetaDispatcher{
		Inner:  &panicInner{boom: "danger.boom"},
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}

	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": powerActions("safe.a", "danger.boom")},
	})
	_ = decodeJSONOutcome(t, out) // envelope still completes

	logged := buf.String()
	if !strings.Contains(logged, "panicked") {
		t.Errorf("panic was not logged : %q", logged)
	}
	if !strings.Contains(logged, "stack") {
		t.Errorf("stack trace was not logged : %q", logged)
	}
}
