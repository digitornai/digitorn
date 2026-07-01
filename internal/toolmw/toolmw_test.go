package toolmw

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func okRes(data string) tool.Result  { return tool.Result{Success: true, Data: data} }
func failRes(msg string) tool.Result { return tool.Result{Success: false, Error: msg} }

// term is a configurable terminal that counts how often it is reached.
type term struct {
	calls int
	res   tool.Result
	err   error
	fn    func(ctx context.Context, cc CallContext) (tool.Result, error)
}

func (t *term) next(ctx context.Context, cc CallContext) (tool.Result, error) {
	t.calls++
	if t.fn != nil {
		return t.fn(ctx, cc)
	}
	return t.res, t.err
}

func cc(session string) CallContext {
	return CallContext{AppID: "app", SessionID: session, ModuleID: "mod", ToolName: "do", Params: []byte(`{"x":1}`), Attempt: 1}
}

func mustMW(t *testing.T, ctor constructor, cfg map[string]any, deps Deps) Middleware {
	t.Helper()
	deps.Logger = nil
	mw, err := ctor(cfg, deps)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	return mw
}

// ---- retry ---------------------------------------------------------------

func TestRetry_TransportErrorRetriesThenGivesUp(t *testing.T) {
	mw := mustMW(t, newRetry, map[string]any{"max_attempts": 3, "base_delay": 0.0}, Deps{})
	tm := &term{res: failRes("boom"), err: errors.New("transport")}
	_, err := mw.Handle(context.Background(), cc("s1"), tm.next)
	if err == nil {
		t.Fatal("expected the transport error to propagate")
	}
	if tm.calls != 3 {
		t.Errorf("retry must exhaust 3 attempts on transport error, got %d", tm.calls)
	}
}

func TestRetry_ModuleFailureIsNotRetried(t *testing.T) {
	mw := mustMW(t, newRetry, map[string]any{"max_attempts": 3, "base_delay": 0.0}, Deps{})
	tm := &term{res: failRes("bad input"), err: nil} // deterministic module failure
	res, err := mw.Handle(context.Background(), cc("s1"), tm.next)
	if err != nil || res.Success {
		t.Fatalf("module failure should pass through, got (success=%v, err=%v)", res.Success, err)
	}
	if tm.calls != 1 {
		t.Errorf("a deterministic Success=false must NOT be retried, got %d calls", tm.calls)
	}
}

func TestRetry_SucceedsOnSecondAttempt(t *testing.T) {
	mw := mustMW(t, newRetry, map[string]any{"max_attempts": 3, "base_delay": 0.0}, Deps{})
	tm := &term{fn: func(_ context.Context, _ CallContext) (tool.Result, error) {
		return tool.Result{}, errors.New("flaky")
	}}
	tm.fn = func() func(context.Context, CallContext) (tool.Result, error) {
		n := 0
		return func(_ context.Context, _ CallContext) (tool.Result, error) {
			n++
			if n < 2 {
				return failRes("flaky"), errors.New("flaky")
			}
			return okRes("ok"), nil
		}
	}()
	res, err := mw.Handle(context.Background(), cc("s1"), tm.next)
	if err != nil || !res.Success {
		t.Fatalf("expected success on retry, got (success=%v, err=%v)", res.Success, err)
	}
	if tm.calls != 2 {
		t.Errorf("expected 2 attempts, got %d", tm.calls)
	}
}

// ---- timeout -------------------------------------------------------------

func TestTimeout_SlowCallTimesOut(t *testing.T) {
	mw := mustMW(t, newTimeout, map[string]any{"seconds": 0.02}, Deps{})
	tm := &term{fn: func(ctx context.Context, _ CallContext) (tool.Result, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return okRes("late"), nil
		case <-ctx.Done():
			return failRes("cancelled"), ctx.Err()
		}
	}}
	_, err := mw.Handle(context.Background(), cc("s1"), tm.next)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestTimeout_FastCallPasses(t *testing.T) {
	mw := mustMW(t, newTimeout, map[string]any{"seconds": 5.0}, Deps{})
	tm := &term{res: okRes("fast")}
	res, err := mw.Handle(context.Background(), cc("s1"), tm.next)
	if err != nil || !res.Success {
		t.Fatalf("fast call must pass, got (success=%v, err=%v)", res.Success, err)
	}
}

// ---- circuit_breaker -----------------------------------------------------

func TestCircuitBreaker_OpensAndBlocks(t *testing.T) {
	mw := mustMW(t, newCircuitBreaker, map[string]any{"failure_threshold": 2, "recovery_timeout": 60.0}, Deps{})
	tm := &term{res: failRes("dead"), err: errors.New("transport")}
	for i := 0; i < 2; i++ {
		_, _ = mw.Handle(context.Background(), cc("s1"), tm.next)
	}
	// Breaker is open now ; a third call from ANY session is blocked without
	// reaching the module.
	_, err := mw.Handle(context.Background(), cc("s2"), tm.next)
	if err == nil {
		t.Fatal("expected the open circuit to block the call")
	}
	if tm.calls != 2 {
		t.Errorf("open breaker must not reach the module, got %d calls", tm.calls)
	}
}

func TestCircuitBreaker_RecoversAfterTimeout(t *testing.T) {
	mw := mustMW(t, newCircuitBreaker, map[string]any{"failure_threshold": 1, "recovery_timeout": 0.02, "half_open_calls": 1}, Deps{})
	bad := &term{res: failRes("dead"), err: errors.New("transport")}
	_, _ = mw.Handle(context.Background(), cc("s1"), bad.next) // trips open
	time.Sleep(40 * time.Millisecond)
	good := &term{res: okRes("alive")}
	res, err := mw.Handle(context.Background(), cc("s1"), good.next)
	if err != nil || !res.Success {
		t.Fatalf("breaker must allow a probe after recovery, got (success=%v, err=%v)", res.Success, err)
	}
}

// ---- dedup : per-session isolation ---------------------------------------

func TestDedup_SameSessionHitDifferentSessionMiss(t *testing.T) {
	mw := mustMW(t, newDedup, map[string]any{"window_seconds": 60.0}, Deps{})
	tm := &term{res: okRes("A-result")}

	_, _ = mw.Handle(context.Background(), cc("sessA"), tm.next) // miss, runs + caches
	_, _ = mw.Handle(context.Background(), cc("sessA"), tm.next) // hit, no run
	if tm.calls != 1 {
		t.Errorf("identical call in same session must be deduped, got %d runs", tm.calls)
	}
	// A DIFFERENT session with identical tool+params must NOT see sessA's
	// cached result — isolation.
	_, _ = mw.Handle(context.Background(), cc("sessB"), tm.next)
	if tm.calls != 2 {
		t.Errorf("dedup must be per-session: sessB must run its own call, got %d runs", tm.calls)
	}
}

// ---- semantic_cache : per-session isolation ------------------------------

type fakeEmbedder struct{ vec map[string][]float32 }

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	for k, v := range f.vec {
		if strings.Contains(text, k) {
			return v, nil
		}
	}
	return []float32{1, 0, 0}, nil
}

func TestSemanticCache_HitSameSessionMissOtherSession(t *testing.T) {
	emb := fakeEmbedder{vec: map[string][]float32{"do": {1, 0, 0}}}
	mw := mustMW(t, newSemanticCache, map[string]any{"similarity_threshold": 0.9, "ttl": 60.0}, Deps{Embedder: emb})
	tm := &term{res: okRes("cached")}

	_, _ = mw.Handle(context.Background(), cc("sessA"), tm.next) // miss, caches
	_, _ = mw.Handle(context.Background(), cc("sessA"), tm.next) // hit (sim 1.0)
	if tm.calls != 1 {
		t.Errorf("semantically identical call in same session must hit cache, got %d runs", tm.calls)
	}
	_, _ = mw.Handle(context.Background(), cc("sessB"), tm.next) // other session: miss
	if tm.calls != 2 {
		t.Errorf("semantic_cache must be per-session, got %d runs", tm.calls)
	}
}

func TestSemanticCache_InertWithoutEmbedder(t *testing.T) {
	mw := mustMW(t, newSemanticCache, nil, Deps{})
	tm := &term{res: okRes("x")}
	_, _ = mw.Handle(context.Background(), cc("s1"), tm.next)
	_, _ = mw.Handle(context.Background(), cc("s1"), tm.next)
	if tm.calls != 2 {
		t.Errorf("without an embedder semantic_cache must be inert, got %d runs", tm.calls)
	}
}

// ---- budget --------------------------------------------------------------

func TestBudget_RejectsOverCap(t *testing.T) {
	mw := mustMW(t, newBudget, map[string]any{"max_calls_per_hour": 2}, Deps{})
	tm := &term{res: okRes("ok")}
	for i := 0; i < 2; i++ {
		if _, err := mw.Handle(context.Background(), cc("s1"), tm.next); err != nil {
			t.Fatalf("call %d must pass under budget: %v", i, err)
		}
	}
	if _, err := mw.Handle(context.Background(), cc("s1"), tm.next); err == nil {
		t.Error("the 3rd call must be rejected over the 2/hour cap")
	}
	if tm.calls != 2 {
		t.Errorf("a rejected call must not reach the module, got %d runs", tm.calls)
	}
}

// ---- auto_heal -----------------------------------------------------------

func TestAutoHeal_AppendsSuggestionsOnFailure(t *testing.T) {
	resolver := func(moduleID, toolName string) []ToolSuggestion {
		return []ToolSuggestion{{ModuleID: moduleID, ToolName: "do_alt", Description: "try this"}}
	}
	mw := mustMW(t, newAutoHeal, nil, Deps{ToolResolver: resolver})
	tm := &term{res: failRes("not found")}
	res, _ := mw.Handle(context.Background(), cc("s1"), tm.next)
	if !strings.Contains(res.Error, "do_alt") {
		t.Errorf("auto_heal must append suggestions to a failed result, got %q", res.Error)
	}
}

func TestAutoHeal_NoSuggestionsOnSuccess(t *testing.T) {
	resolver := func(string, string) []ToolSuggestion { return []ToolSuggestion{{ToolName: "x"}} }
	mw := mustMW(t, newAutoHeal, nil, Deps{ToolResolver: resolver})
	tm := &term{res: okRes("fine")}
	res, _ := mw.Handle(context.Background(), cc("s1"), tm.next)
	if res.Data != "fine" {
		t.Errorf("a successful result must be untouched, got %v", res.Data)
	}
}

// ---- cross_context : per-session trail via ctx ---------------------------

func TestCrossContext_TrailIsPerSessionAndVisible(t *testing.T) {
	mw := mustMW(t, newCrossContext, map[string]any{"max_entries": 10}, Deps{})
	var seen []RecentCall
	tm := &term{fn: func(ctx context.Context, _ CallContext) (tool.Result, error) {
		seen = RecentContext(ctx)
		return okRes("out"), nil
	}}

	_, _ = mw.Handle(context.Background(), cc("sessA"), tm.next) // records; no prior trail
	if len(seen) != 0 {
		t.Errorf("first call must see an empty trail, got %d", len(seen))
	}
	_, _ = mw.Handle(context.Background(), cc("sessA"), tm.next) // sees its own prior output
	if len(seen) != 1 || seen[0].ToolName != "do" {
		t.Errorf("second same-session call must see the recorded trail, got %+v", seen)
	}
	_, _ = mw.Handle(context.Background(), cc("sessB"), tm.next) // other session: empty trail
	if len(seen) != 0 {
		t.Errorf("cross_context trail must be per-session, sessB saw %d entries", len(seen))
	}
}

// ---- pipeline composition ------------------------------------------------

func TestPipeline_OnionOrderAndIdentity(t *testing.T) {
	var order []string
	outer := recorder{name: "outer", log: &order}
	inner := recorder{name: "inner", log: &order}
	p := New([]Middleware{outer, inner}, nil)

	ctx := tool.WithIdentity(context.Background(), tool.Identity{SessionID: "s1", ModuleID: "mod", ToolName: "do"})
	got, err := p.Run(ctx, []byte(`{}`), func(context.Context) (tool.Result, error) {
		order = append(order, "terminal")
		return okRes("done"), nil
	})
	if err != nil || got.Data != "done" {
		t.Fatalf("pipeline run failed: (%v, %v)", got, err)
	}
	want := []string{"outer-pre", "inner-pre", "terminal", "inner-post", "outer-post"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("onion order wrong:\n got %v\nwant %v", order, want)
	}
}

type recorder struct {
	name string
	log  *[]string
}

func (r recorder) Name() string { return r.name }
func (r recorder) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	*r.log = append(*r.log, r.name+"-pre")
	res, err := next(ctx, cc)
	*r.log = append(*r.log, r.name+"-post")
	return res, err
}

// ---- Build ---------------------------------------------------------------

func TestBuild_ParsesBothFormsAndSkipsDisabled(t *testing.T) {
	entries := []map[string]any{
		{"retry": map[string]any{"max_attempts": 2}},  // name-as-key
		{"name": "audit", "config": map[string]any{}}, // structured
		{"name": "timeout", "enabled": false},         // disabled → skipped
		{"name": "nope"},                              // unknown → skipped
	}
	p := Build(entries, Deps{}, nil)
	if p == nil {
		t.Fatal("expected a pipeline")
	}
	got := strings.Join(p.Names(), ",")
	if got != "retry,audit" {
		t.Errorf("Build must keep enabled known middleware in order, got %q", got)
	}
}

func TestBuild_EmptyReturnsNil(t *testing.T) {
	if p := Build(nil, Deps{}, nil); p != nil {
		t.Error("no entries must yield a nil pipeline (fast path)")
	}
}
