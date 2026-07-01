package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// fakeBus is a paranoid, controllable ports.ServiceBus stub.
// It records every Call, lets tests force errors / results, and
// proves the adapter handles concurrent dispatch correctly.
// =====================================================================

type recordedCall struct {
	ModuleID string
	ToolName string
	Params   []byte
	Ctx      context.Context
}

type fakeBus struct {
	mu sync.Mutex

	calls []recordedCall

	// Optional reaction overrides. nil = use defaultResult.
	react func(moduleID, toolName string, params []byte) (tool.Result, error)

	defaultResult tool.Result
	defaultErr    error

	// CallCount is incremented atomically for racy tests.
	callCount int64

	// Block makes Call wait on a channel before returning ; used by
	// the cancellation tests.
	block chan struct{}
}

func (f *fakeBus) Register(_ module.Module) error     { return nil }
func (f *fakeBus) Unregister(_ string) error          { return nil }
func (f *fakeBus) Get(_ string) (module.Module, bool) { return nil, false }
func (f *fakeBus) List() []module.Module              { return nil }

func (f *fakeBus) Call(ctx context.Context, moduleID, toolName string, params []byte) (tool.Result, error) {
	atomic.AddInt64(&f.callCount, 1)

	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{
		ModuleID: moduleID,
		ToolName: toolName,
		Params:   append([]byte(nil), params...),
		Ctx:      ctx,
	})
	react := f.react
	defRes := f.defaultResult
	defErr := f.defaultErr
	block := f.block
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return tool.Result{Success: false, Error: ctx.Err().Error()}, ctx.Err()
		}
	}

	if react != nil {
		return react(moduleID, toolName, params)
	}
	return defRes, defErr
}

// =====================================================================
// 1. Constructor
// =====================================================================

func TestNewBusAdapter_NilBusReturnsNil(t *testing.T) {
	a := dispatch.NewBusAdapter(nil)
	if a != nil {
		t.Fatalf("NewBusAdapter(nil) = %v, want nil", a)
	}
}

func TestNewBusAdapter_RealBusReturnsAdapter(t *testing.T) {
	a := dispatch.NewBusAdapter(&fakeBus{})
	if a == nil {
		t.Fatal("NewBusAdapter(bus) = nil, want non-nil")
	}
	if a.Bus == nil {
		t.Errorf("adapter.Bus = nil after construction")
	}
}

// =====================================================================
// 2. FQN parsing
// =====================================================================

func TestDispatch_RejectsNameWithoutDot(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	a := dispatch.NewBusAdapter(bus)

	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "noDot"})
	if out.Status != "errored" {
		t.Errorf("status = %q, want errored", out.Status)
	}
	if !strings.Contains(out.Error, "module.action") {
		t.Errorf("error = %q, want hint about module.action form", out.Error)
	}
	if atomic.LoadInt64(&bus.callCount) != 0 {
		t.Errorf("bus.Call was invoked for malformed name")
	}
}

func TestDispatch_RejectsTrailingDot(t *testing.T) {
	bus := &fakeBus{}
	a := dispatch.NewBusAdapter(bus)
	for _, name := range []string{"filesystem.", ".read", "", "."} {
		out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: name})
		if out.Status != "errored" {
			t.Errorf("name=%q : status = %q, want errored", name, out.Status)
		}
	}
	if atomic.LoadInt64(&bus.callCount) != 0 {
		t.Errorf("bus.Call was invoked for malformed names")
	}
}

func TestDispatch_SplitsOnFirstDotOnly(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: "ok"}}
	a := dispatch.NewBusAdapter(bus)
	_ = a.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "ns.sub.action",
		Args: map[string]any{"k": "v"},
	})
	if len(bus.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(bus.calls))
	}
	c := bus.calls[0]
	if c.ModuleID != "ns" || c.ToolName != "sub.action" {
		t.Errorf("split wrong : module=%q, tool=%q ; want ns / sub.action", c.ModuleID, c.ToolName)
	}
}

// =====================================================================
// 3. Args marshalling
// =====================================================================

func TestDispatch_NilArgsEncodesAsNull(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	a := dispatch.NewBusAdapter(bus)
	a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a", Args: nil})
	if len(bus.calls) != 1 {
		t.Fatalf("calls = %d", len(bus.calls))
	}
	if string(bus.calls[0].Params) != "null" {
		t.Errorf("nil args → params = %q, want null", bus.calls[0].Params)
	}
}

func TestDispatch_EmptyArgsEncodesAsEmptyObject(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	a := dispatch.NewBusAdapter(bus)
	a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a", Args: map[string]any{}})
	if string(bus.calls[0].Params) != "{}" {
		t.Errorf("empty args → params = %q, want {}", bus.calls[0].Params)
	}
}

func TestDispatch_RoundtripsComplexArgs(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	a := dispatch.NewBusAdapter(bus)
	args := map[string]any{
		"path":    "/tmp/foo",
		"offset":  float64(42),
		"flags":   []any{"r", "w"},
		"nested":  map[string]any{"x": "y"},
		"unicode": "café 日本語 🦀",
	}
	a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a", Args: args})
	if len(bus.calls) != 1 {
		t.Fatalf("calls = %d", len(bus.calls))
	}
	var decoded map[string]any
	if err := json.Unmarshal(bus.calls[0].Params, &decoded); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, bus.calls[0].Params)
	}
	if decoded["path"] != "/tmp/foo" {
		t.Errorf("path lost: %v", decoded["path"])
	}
	if decoded["unicode"] != "café 日本語 🦀" {
		t.Errorf("unicode lost: %v", decoded["unicode"])
	}
}

func TestDispatch_RejectsUnmarshallableArgs(t *testing.T) {
	bus := &fakeBus{}
	a := dispatch.NewBusAdapter(bus)
	// json cannot encode NaN
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "m.a", Args: map[string]any{"x": math.NaN()},
	})
	if out.Status != "errored" {
		t.Errorf("status = %q, want errored", out.Status)
	}
	if !strings.Contains(out.Error, "marshal args") {
		t.Errorf("error = %q, want 'marshal args' substring", out.Error)
	}
	if atomic.LoadInt64(&bus.callCount) != 0 {
		t.Errorf("bus.Call should not be invoked when args are unencodable")
	}
}

// =====================================================================
// 4. Bus errors / non-success results
// =====================================================================

func TestDispatch_BusErrorBecomesErrored(t *testing.T) {
	bus := &fakeBus{defaultErr: errors.New("module not found")}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Status != "errored" {
		t.Errorf("status = %q, want errored", out.Status)
	}
	if !strings.Contains(out.Error, "module not found") {
		t.Errorf("error = %q, missing wrapped bus err", out.Error)
	}
}

func TestDispatch_NonSuccessResultBecomesErrored(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: false, Error: "permission denied"}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Status != "errored" {
		t.Errorf("status = %q", out.Status)
	}
	if !strings.Contains(out.Error, "permission denied") {
		t.Errorf("error = %q, want 'permission denied'", out.Error)
	}
}

func TestDispatch_NonSuccessNoMessageGetsSentinel(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: false}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Status != "errored" || out.Error == "" {
		t.Errorf("want errored with non-empty msg, got status=%q err=%q", out.Status, out.Error)
	}
}

func TestDispatch_FailedResultIncludesOutputParts(t *testing.T) {
	// A failed tool's Data (a command's stdout/stderr, a validation message)
	// must reach the LLM, not just the error string — otherwise the model is
	// blind to WHY it failed (it only sees "exit code 1").
	bus := &fakeBus{defaultResult: tool.Result{
		Success: false,
		Error:   "exit code 1",
		Data:    map[string]any{"stderr": "npm ERR! missing package.json", "exit_code": 1},
	}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "bash.run"})
	if out.Status != "errored" {
		t.Fatalf("status = %q", out.Status)
	}
	if !strings.Contains(out.Error, "exit code 1") {
		t.Fatalf("error = %q", out.Error)
	}
	if len(out.Parts) == 0 || !strings.Contains(out.Parts[0].Text, "npm ERR! missing package.json") {
		t.Fatalf("failed result did not deliver its output to the LLM: %+v", out.Parts)
	}
}

// =====================================================================
// 5. Successful result → Parts
// =====================================================================

func TestDispatch_StringDataPassesThrough(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: "hello world"}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Status != "completed" {
		t.Fatalf("status = %q", out.Status)
	}
	if len(out.Parts) != 1 || out.Parts[0].Type != sessionstore.PartTypeText {
		t.Fatalf("parts = %+v", out.Parts)
	}
	if out.Parts[0].Text != "hello world" {
		t.Errorf("text = %q, want 'hello world'", out.Parts[0].Text)
	}
}

// TestDispatch_DiffAndMetadataAreClientOnly proves the dispatch boundary carries
// the diff view + metadata onto the ToolOutcome (→ client) but keeps them OUT of
// Parts (the only thing the LLM adapter projects into the model context).
func TestDispatch_DiffAndMetadataAreClientOnly(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{
		Success:  true,
		Data:     "Edited f.go (1 replacement)",
		Metadata: map[string]any{"bytes_written": 42},
		Diff: &tool.DiffView{
			Unified:         "--- a/f.go\n+++ b/f.go\n@@ -1,1 +1,1 @@\n-old\n+new\n",
			Summary:         "+1 −1",
			PreviousContent: "old\n",
			NewContent:      "new\n",
			Additions:       1,
			Deletions:       1,
		},
	}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "filesystem.edit"})

	if out.Diff == nil || out.Diff.Unified == "" {
		t.Fatal("diff view must reach the outcome (client channel)")
	}
	if out.Metadata["bytes_written"] != 42 {
		t.Errorf("metadata must reach the outcome: %+v", out.Metadata)
	}
	// The LLM-visible Parts must be ONLY the summary text — no unified diff,
	// no previous/new content.
	if len(out.Parts) != 1 || out.Parts[0].Text != "Edited f.go (1 replacement)" {
		t.Fatalf("parts must carry only the summary text, got %+v", out.Parts)
	}
	for _, p := range out.Parts {
		if strings.Contains(p.Text, "@@") || strings.Contains(p.Text, "+new") || strings.Contains(p.Text, "previous_content") {
			t.Errorf("diff leaked into LLM-visible Parts: %q", p.Text)
		}
	}
}

func TestDispatch_BytesDataDecodedAsUTF8(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: []byte("byte payload")}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Parts[0].Text != "byte payload" {
		t.Errorf("bytes → text = %q", out.Parts[0].Text)
	}
}

func TestDispatch_StructDataJSONIndented(t *testing.T) {
	type entry struct {
		Name string `json:"name"`
		Size int    `json:"size"`
	}
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: []entry{{"a", 1}, {"b", 2}}}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if !strings.Contains(out.Parts[0].Text, `"name": "a"`) {
		t.Errorf("expected indented JSON, got %q", out.Parts[0].Text)
	}
	if !strings.Contains(out.Parts[0].Text, "\n") {
		t.Errorf("expected newlines from indent, got %q", out.Parts[0].Text)
	}
}

func TestDispatch_NilDataIsEmptyString(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: nil}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Parts[0].Text != "" {
		t.Errorf("nil data → text = %q, want empty", out.Parts[0].Text)
	}
}

// =====================================================================
// 6. Routing context propagation
// =====================================================================

func TestDispatch_PassesContextToBus(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	a := dispatch.NewBusAdapter(bus)
	type ctxKey string
	const k ctxKey = "sentinel"
	parent := context.WithValue(context.Background(), k, "marker")
	a.Dispatch(parent, runtime.ToolInvocation{Name: "m.a"})
	got := bus.calls[0].Ctx.Value(k)
	if got != "marker" {
		t.Errorf("ctx not propagated : value = %v", got)
	}
}

func TestDispatch_CancellationStopsBlockedCall(t *testing.T) {
	block := make(chan struct{})
	bus := &fakeBus{
		block:         block,
		defaultResult: tool.Result{Success: true},
	}
	a := dispatch.NewBusAdapter(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan runtime.ToolOutcome, 1)
	go func() {
		done <- a.Dispatch(ctx, runtime.ToolInvocation{Name: "m.a"})
	}()

	// give the call time to land at the block
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case out := <-done:
		if out.Status != "errored" {
			t.Errorf("cancel → status = %q, want errored", out.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after ctx cancel")
	}
	close(block) // release the (already-returned) call
}

// =====================================================================
// 7. DurationMs invariants
// =====================================================================

func TestDispatch_DurationAlwaysPopulated(t *testing.T) {
	// success path
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.DurationMs < 0 {
		t.Errorf("success duration = %d (negative)", out.DurationMs)
	}

	// error path
	bus2 := &fakeBus{defaultErr: errors.New("boom")}
	a2 := dispatch.NewBusAdapter(bus2)
	out2 := a2.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out2.DurationMs < 0 {
		t.Errorf("error duration = %d (negative)", out2.DurationMs)
	}

	// malformed-name path
	bus3 := &fakeBus{}
	a3 := dispatch.NewBusAdapter(bus3)
	out3 := a3.Dispatch(context.Background(), runtime.ToolInvocation{Name: "bad"})
	if out3.DurationMs < 0 {
		t.Errorf("bad-name duration = %d (negative)", out3.DurationMs)
	}
}

func TestDispatch_NowFnPinsDuration(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true}}
	step := int64(0)
	a := &dispatch.BusAdapter{
		Bus: bus,
		NowFn: func() time.Time {
			n := atomic.AddInt64(&step, 1)
			// 1st call: t=0ms. 2nd call: t=42ms.
			return time.Unix(0, (n-1)*42*int64(time.Millisecond))
		},
	}
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", out.DurationMs)
	}
}

// =====================================================================
// 8. Concurrency / isolation
// =====================================================================

func TestDispatch_ConcurrentCallsAllReachBus(t *testing.T) {
	const N = 500
	bus := &fakeBus{
		react: func(_, _ string, _ []byte) (tool.Result, error) {
			return tool.Result{Success: true, Data: "ok"}, nil
		},
	}
	a := dispatch.NewBusAdapter(bus)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			out := a.Dispatch(context.Background(), runtime.ToolInvocation{
				Name: "m.a",
				Args: map[string]any{"i": i},
			})
			if out.Status != "completed" {
				t.Errorf("[%d] status = %q", i, out.Status)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&bus.callCount); got != N {
		t.Errorf("bus calls = %d, want %d", got, N)
	}
}

func TestDispatch_ConcurrentCallsCarryOwnArgs(t *testing.T) {
	const N = 200
	bus := &fakeBus{
		react: func(_, _ string, params []byte) (tool.Result, error) {
			return tool.Result{Success: true, Data: string(params)}, nil
		},
	}
	a := dispatch.NewBusAdapter(bus)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			out := a.Dispatch(context.Background(), runtime.ToolInvocation{
				Name: "m.a",
				Args: map[string]any{"i": i},
			})
			want := fmt.Sprintf(`"i":%d`, i)
			if !strings.Contains(out.Parts[0].Text, want) {
				t.Errorf("[%d] response = %q, missing %q", i, out.Parts[0].Text, want)
			}
		}(i)
	}
	wg.Wait()
}

// =====================================================================
// 9. Error message hygiene
// =====================================================================

func TestDispatch_ErrorMessagesIncludeToolName(t *testing.T) {
	bus := &fakeBus{defaultErr: errors.New("boom")}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "filesystem.read"})
	if !strings.Contains(out.Error, "filesystem.read") {
		t.Errorf("error %q must include tool name for diagnosability", out.Error)
	}
}

func TestDispatch_BusErrAndResultErrBothIncluded(t *testing.T) {
	bus := &fakeBus{
		defaultErr:    errors.New("module not found"),
		defaultResult: tool.Result{Success: false, Error: "wrapped detail"},
	}
	a := dispatch.NewBusAdapter(bus)
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if !strings.Contains(out.Error, "module not found") {
		t.Errorf("missing bus err: %q", out.Error)
	}
	if !strings.Contains(out.Error, "wrapped detail") {
		t.Errorf("missing result err: %q", out.Error)
	}
}

// =====================================================================
// 10. Nil safety
// =====================================================================

func TestDispatch_NilBusField(t *testing.T) {
	a := &dispatch.BusAdapter{Bus: nil}
	out := a.Dispatch(context.Background(), runtime.ToolInvocation{Name: "m.a"})
	if out.Status != "errored" || !strings.Contains(out.Error, "bus is nil") {
		t.Errorf("nil-bus dispatch: status=%q err=%q", out.Status, out.Error)
	}
}
