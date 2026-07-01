package meta_test

import (
	"context"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
)

// =====================================================================
// PL-1 — Lock the 3 meta primitives that resolve a sub-tool name
// (execute_tool, run_parallel, background_run) against the SubToolGate.
//
// Security model under test (per the project's stated rule) :
//
//   - Meta-tools themselves bypass security entirely. That bypass lives
//     in the real gate (policy.RunGates), NOT in the MetaDispatcher : the
//     dispatcher calls gateTarget on EVERY resolved sub-tool and lets the
//     gate decide. So at this layer we assert the chokepoint is wired :
//     the gate is consulted for each resolved sub-tool, and a deny stops
//     the call before it reaches the inner dispatcher / background mgr.
//   - When no gate is wired (dev/test), every path runs unguarded.
//
// The runtime-level proof that real capabilities.deny is enforced
// through these paths (with the real Engine gate) lives in
// internal/runtime/pl1_meta_gate_lock_test.go.
// =====================================================================

// recordingGate is a stub SubToolGate that denies a fixed set of tool
// FQNs and records every invocation it was asked to evaluate, so the
// lock tests can assert (a) the path consulted the gate and (b) a denied
// sub-tool never reached the inner dispatcher / manager.
type recordingGate struct {
	mu     sync.Mutex
	denied map[string]bool
	seen   []runtime.ToolInvocation
}

func newRecordingGate(deny ...string) *recordingGate {
	m := make(map[string]bool, len(deny))
	for _, d := range deny {
		m[d] = true
	}
	return &recordingGate{denied: m}
}

func (g *recordingGate) GateSubTool(_ context.Context, inv runtime.ToolInvocation) *runtime.ToolOutcome {
	g.mu.Lock()
	g.seen = append(g.seen, inv)
	denied := g.denied[inv.Name]
	g.mu.Unlock()
	if denied {
		return &runtime.ToolOutcome{
			Status: "errored",
			Error:  "denied by security policy: " + inv.Name,
		}
	}
	return nil
}

func (g *recordingGate) evaluatedNames() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.seen))
	for i := range g.seen {
		out[i] = g.seen[i].Name
	}
	return out
}

func (g *recordingGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}

// innerNames returns the canonical names the inner dispatcher actually
// received, read after wg.Wait so there's no concurrent access.
func innerNames(e *echoInner) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.calls))
	for i := range e.calls {
		out[i] = e.calls[i].Name
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// =====================================================================
// execute_tool
// =====================================================================

func TestGateLock_ExecuteTool_DeniedBlockedBeforeInner(t *testing.T) {
	gate := newRecordingGate("shell.bash")
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner, Gate: gate}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name":   "shell.bash",
			"params": map[string]any{"cmd": "rm -rf /"},
		},
	})

	if out.Status != "errored" {
		t.Fatalf("denied execute_tool: status=%q want errored", out.Status)
	}
	if n := len(innerNames(inner)); n != 0 {
		t.Errorf("inner reached %d times for denied sub-tool, want 0", n)
	}
	if got := gate.evaluatedNames(); len(got) != 1 || got[0] != "shell.bash" {
		t.Errorf("gate evaluated %v, want exactly [shell.bash]", got)
	}
}

func TestGateLock_ExecuteTool_AllowedReachesInner(t *testing.T) {
	gate := newRecordingGate("shell.bash") // shell denied, filesystem allowed
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner, Gate: gate}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name":   "filesystem.read",
			"params": map[string]any{"path": "/etc/hosts"},
		},
	})

	if out.Status != "completed" {
		t.Fatalf("allowed execute_tool: status=%q err=%q", out.Status, out.Error)
	}
	got := innerNames(inner)
	if len(got) != 1 || got[0] != "filesystem.read" {
		t.Errorf("inner saw %v, want [filesystem.read]", got)
	}
	if gate.count() != 1 {
		t.Errorf("gate consulted %d times, want 1", gate.count())
	}
}

// =====================================================================
// run_parallel
// =====================================================================

func TestGateLock_RunParallel_OnlyAllowedReachInner(t *testing.T) {
	gate := newRecordingGate("danger.delete")
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner, Gate: gate}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			map[string]any{"name": "safe.a", "params": map[string]any{}},
			map[string]any{"name": "danger.delete", "params": map[string]any{}},
			map[string]any{"name": "safe.c", "params": map[string]any{}},
		}},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results len = %d, want 3", len(results))
	}
	// Order is preserved : the denied action sits at index 1 and is
	// errored ; the other two completed.
	r1 := results[1].(map[string]any)
	if r1["name"] != "danger.delete" || r1["status"] != "errored" {
		t.Errorf("results[1] = %v, want denied danger.delete errored", r1)
	}
	for _, i := range []int{0, 2} {
		if results[i].(map[string]any)["status"] != "completed" {
			t.Errorf("results[%d] status = %v, want completed", i, results[i])
		}
	}
	// Inner only ran the two allowed children, never the denied one.
	got := innerNames(inner)
	if len(got) != 2 {
		t.Fatalf("inner reached %d times, want 2 (only allowed)", len(got))
	}
	if containsName(got, "danger.delete") {
		t.Errorf("denied sub-tool reached inner : %v", got)
	}
	if gate.count() != 3 {
		t.Errorf("gate consulted %d times, want 3 (one per child)", gate.count())
	}
}

func TestGateLock_RunParallel_AllDeniedNeverReachInner(t *testing.T) {
	gate := newRecordingGate("danger.a", "danger.b")
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner, Gate: gate}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			map[string]any{"name": "danger.a", "params": map[string]any{}},
			map[string]any{"name": "danger.b", "params": map[string]any{}},
		}},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	for i, raw := range results {
		if raw.(map[string]any)["status"] != "errored" {
			t.Errorf("results[%d] not errored : %v", i, raw)
		}
	}
	if n := len(innerNames(inner)); n != 0 {
		t.Errorf("inner reached %d times, want 0 (all denied)", n)
	}
}

// TestGateLock_RunParallel_ConcurrentDenyEnforcementRaceClean fans out a
// large mixed batch so the -race detector exercises the per-child gate +
// inner goroutines. Exactly the allowed children must reach the inner ;
// every denied one is blocked.
func TestGateLock_RunParallel_ConcurrentDenyEnforcementRaceClean(t *testing.T) {
	gate := newRecordingGate("blocked.tool")
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner, Gate: gate}

	const n = 40
	actions := make([]any, n)
	wantAllowed := 0
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			actions[i] = map[string]any{"name": "allowed.tool", "params": map[string]any{"i": float64(i)}}
			wantAllowed++
		} else {
			actions[i] = map[string]any{"name": "blocked.tool", "params": map[string]any{"i": float64(i)}}
		}
	}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": actions},
	})

	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if len(results) != n {
		t.Fatalf("results len = %d, want %d", len(results), n)
	}
	for i, raw := range results {
		st := raw.(map[string]any)["status"]
		if i%2 == 0 && st != "completed" {
			t.Errorf("results[%d] (allowed) status = %v, want completed", i, st)
		}
		if i%2 == 1 && st != "errored" {
			t.Errorf("results[%d] (denied) status = %v, want errored", i, st)
		}
	}
	got := innerNames(inner)
	if len(got) != wantAllowed {
		t.Errorf("inner reached %d times, want %d (allowed only)", len(got), wantAllowed)
	}
	if containsName(got, "blocked.tool") {
		t.Error("blocked.tool reached the inner dispatcher")
	}
	if gate.count() != n {
		t.Errorf("gate consulted %d times, want %d", gate.count(), n)
	}
}

// =====================================================================
// background_run
// =====================================================================

func TestGateLock_BackgroundRun_DeniedNeverLaunches(t *testing.T) {
	gate := newRecordingGate("shell.bash")
	bg := &fakeBg{}
	d := &meta.MetaDispatcher{Background: bg, Gate: gate}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{
			"name":   "shell.bash",
			"params": map[string]any{"cmd": "curl evil"},
		},
	})

	if out.Status != "errored" {
		t.Fatalf("denied background_run: status=%q want errored", out.Status)
	}
	if bg.launchCalls != 0 {
		t.Errorf("Background.Launch called %d times for denied tool, want 0", bg.launchCalls)
	}
	if got := gate.evaluatedNames(); len(got) != 1 || got[0] != "shell.bash" {
		t.Errorf("gate evaluated %v, want [shell.bash]", got)
	}
}

func TestGateLock_BackgroundRun_AllowedLaunches(t *testing.T) {
	gate := newRecordingGate("shell.bash")
	bg := &fakeBg{}
	d := &meta.MetaDispatcher{Background: bg, Gate: gate}

	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{
			"name":   "filesystem.read",
			"params": map[string]any{"path": "/x"},
		},
	})

	if out.Status != "completed" {
		t.Fatalf("allowed background_run: status=%q err=%q", out.Status, out.Error)
	}
	if bg.launchCalls != 1 {
		t.Errorf("Background.Launch called %d times, want 1", bg.launchCalls)
	}
}

// TestGateLock_BackgroundRun_GateSeesCallerIdentity : the launch target
// must be gated with the CALLER's real identity (app / user / agent),
// not the manager's tenancy key. The manager dispatches the task later
// with a tenancy-key AppID that can't be gated downstream, so this is
// the only place the real identity is available.
func TestGateLock_BackgroundRun_GateSeesCallerIdentity(t *testing.T) {
	gate := newRecordingGate() // allow all, just record
	bg := &fakeBg{}
	d := &meta.MetaDispatcher{Background: bg, Gate: gate}

	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name:      "context_builder.background_run",
		AppID:     "real-app",
		AgentID:   "main",
		UserID:    "u1",
		SessionID: "sess-9",
		Args: map[string]any{
			"name":   "filesystem.read",
			"params": map[string]any{"path": "/x"},
		},
	})

	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.seen) != 1 {
		t.Fatalf("gate saw %d invocations, want 1", len(gate.seen))
	}
	inv := gate.seen[0]
	if inv.Name != "filesystem.read" {
		t.Errorf("gated name = %q, want filesystem.read", inv.Name)
	}
	if inv.AppID != "real-app" || inv.AgentID != "main" || inv.UserID != "u1" {
		t.Errorf("gate lost caller identity : app=%q agent=%q user=%q",
			inv.AppID, inv.AgentID, inv.UserID)
	}
}

// =====================================================================
// dev mode : no gate wired = no enforcement (all 3 paths run unguarded)
// =====================================================================

func TestGateLock_NilGate_DevModeNoEnforcement(t *testing.T) {
	t.Run("execute_tool", func(t *testing.T) {
		inner := &echoInner{}
		d := &meta.MetaDispatcher{Inner: inner} // Gate nil
		out := d.Dispatch(context.Background(), runtime.ToolInvocation{
			Name: "context_builder.execute_tool",
			Args: map[string]any{"name": "shell.bash", "params": map[string]any{}},
		})
		if out.Status != "completed" {
			t.Fatalf("nil-gate execute_tool: status=%q err=%q", out.Status, out.Error)
		}
		if got := innerNames(inner); len(got) != 1 || got[0] != "shell.bash" {
			t.Errorf("inner saw %v, want [shell.bash] (no gate)", got)
		}
	})

	t.Run("run_parallel", func(t *testing.T) {
		inner := &echoInner{}
		d := &meta.MetaDispatcher{Inner: inner} // Gate nil
		d.Dispatch(context.Background(), runtime.ToolInvocation{
			Name: "context_builder.run_parallel",
			Args: map[string]any{"actions": []any{
				map[string]any{"name": "a.x", "params": map[string]any{}},
				map[string]any{"name": "b.y", "params": map[string]any{}},
			}},
		})
		if got := innerNames(inner); len(got) != 2 {
			t.Errorf("inner reached %d times, want 2 (no gate)", len(got))
		}
	})

	t.Run("background_run", func(t *testing.T) {
		bg := &fakeBg{}
		d := &meta.MetaDispatcher{Background: bg} // Gate nil
		out := d.Dispatch(context.Background(), runtime.ToolInvocation{
			Name: "context_builder.background_run",
			Args: map[string]any{"name": "shell.bash", "params": map[string]any{}},
		})
		if out.Status != "completed" {
			t.Fatalf("nil-gate background_run: status=%q err=%q", out.Status, out.Error)
		}
		if bg.launchCalls != 1 {
			t.Errorf("Background.Launch called %d times, want 1 (no gate)", bg.launchCalls)
		}
	})
}
