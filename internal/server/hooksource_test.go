package server

import (
	"context"
	"errors"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
)

// =====================================================================
// stubApps — minimal appmgr.Manager that returns canned RuntimeApps.
// Only Get is exercised by hookSource ; every other method panics so
// any accidental use surfaces immediately.
// =====================================================================

type stubApps struct {
	byID map[string]*appmgr.RuntimeApp
}

func (s *stubApps) Get(_ context.Context, id string) (*appmgr.RuntimeApp, error) {
	if app, ok := s.byID[id]; ok {
		return app, nil
	}
	return nil, errors.New("not found")
}

func (s *stubApps) Install(context.Context, string, string) (*appmgr.App, error) {
	panic("stubApps.Install not used")
}
func (s *stubApps) Upgrade(context.Context, string, string, string) (*appmgr.App, error) {
	panic("stubApps.Upgrade not used")
}
func (s *stubApps) Uninstall(context.Context, string, bool) error { panic("not used") }
func (s *stubApps) Enable(context.Context, string) error          { panic("not used") }
func (s *stubApps) Disable(context.Context, string) error         { panic("not used") }
func (s *stubApps) SetBYOK(context.Context, string, bool) error   { panic("not used") }
func (s *stubApps) Reload(context.Context, string) error          { panic("not used") }
func (s *stubApps) CheckUpdate(context.Context, string, string) (*appmgr.UpdateInfo, error) {
	panic("not used")
}
func (s *stubApps) List(context.Context, bool) ([]appmgr.App, error)    { panic("not used") }
func (s *stubApps) ListDisabled(context.Context) ([]appmgr.App, error)  { panic("not used") }
func (s *stubApps) GetApp(context.Context, string) (*appmgr.App, error) { panic("not used") }
func (s *stubApps) GetManifest(context.Context, string) (*schema.AppDefinition, error) {
	panic("not used")
}
func (s *stubApps) Bootstrap(context.Context) error { return nil }

// =====================================================================
// Helpers
// =====================================================================

func makeApp(id string, runtimeHooks []schema.Hook, agents []schema.Agent) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: id, Enabled: true},
		Definition: &schema.AppDefinition{
			App:     schema.AppMeta{AppID: id, Name: id, Version: "1.0"},
			Runtime: &schema.RuntimeBlock{Hooks: runtimeHooks},
			Agents:  agents,
		},
	}
}

func appHook(id string, on schema.HookEvent) schema.Hook {
	return schema.Hook{
		ID: id, On: on,
		Condition: schema.HookCondition{Type: "always"},
		Action:    schema.HookAction{Type: "noop"},
	}
}

// =====================================================================
// ForApp
// =====================================================================

func TestHookSource_ForApp_BuildsFromRuntimeHooks(t *testing.T) {
	app := makeApp("app-1", []schema.Hook{
		appHook("h1", schema.HookEventTurnStart),
		appHook("h2", schema.HookEventToolEnd),
	}, nil)
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{"app-1": app}},
		hooks.ActionDeps{})

	eng := src.ForApp("app-1")
	if eng == nil {
		t.Fatal("ForApp returned nil for valid app")
	}
	// Runtime-default hooks are prepended ahead of the app's own.
	nb := len(hooks.BuiltinHooks())
	if len(eng.Hooks) != nb+2 {
		t.Errorf("engine has %d hooks, want %d", len(eng.Hooks), nb+2)
	}
	if eng.Hooks[nb].ID != "h1" || eng.Hooks[nb+1].ID != "h2" {
		t.Errorf("app hook order lost : %v", eng.Hooks)
	}
}

func TestHookSource_ForApp_CachesEngineInstance(t *testing.T) {
	// CRITICAL : cooldown / max_fires state depends on the engine
	// surviving across turns. ForApp MUST return the same instance.
	app := makeApp("app-1", []schema.Hook{appHook("h1", schema.HookEventTurnStart)}, nil)
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{"app-1": app}},
		hooks.ActionDeps{})

	first := src.ForApp("app-1")
	second := src.ForApp("app-1")
	if first != second {
		t.Errorf("ForApp returned distinct instances on repeat call (state would reset every turn)")
	}
}

func TestHookSource_ForApp_PerAppIsolation(t *testing.T) {
	a := makeApp("app-A", []schema.Hook{appHook("hA", schema.HookEventTurnStart)}, nil)
	b := makeApp("app-B", []schema.Hook{appHook("hB", schema.HookEventTurnStart)}, nil)
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{
		"app-A": a, "app-B": b,
	}}, hooks.ActionDeps{})

	engA := src.ForApp("app-A")
	engB := src.ForApp("app-B")
	if engA == engB {
		t.Fatal("apps must NOT share their engine instance")
	}
	nb := len(hooks.BuiltinHooks())
	if engA.Hooks[nb].ID != "hA" || engB.Hooks[nb].ID != "hB" {
		t.Errorf("cross-app hook leak : A=%v B=%v", engA.Hooks, engB.Hooks)
	}
}

func TestHookSource_ForApp_NilSafe(t *testing.T) {
	var s *hookSource
	if got := s.ForApp("app-1"); got != nil {
		t.Errorf("nil receiver should return nil engine, got %v", got)
	}
	src := newHookSource(&stubApps{}, hooks.ActionDeps{})
	if got := src.ForApp(""); got != nil {
		t.Errorf("empty appID should return nil engine")
	}
	if got := src.ForApp("missing"); got != nil {
		t.Errorf("unknown app should return nil engine, got %v", got)
	}
}

func TestHookSource_ForApp_NoRuntimeBlock(t *testing.T) {
	// App without runtime.hooks[] still gets an engine — it just
	// has no app-level hooks. Per-agent hooks may still fire.
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "bare"},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "bare", Name: "bare", Version: "1.0"},
		},
	}
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{"bare": app}},
		hooks.ActionDeps{})
	eng := src.ForApp("bare")
	if eng == nil {
		t.Fatal("ForApp returned nil for app with no runtime block")
	}
	// Only the runtime-default hooks are present (no app-level hooks).
	if len(eng.Hooks) != len(hooks.BuiltinHooks()) {
		t.Errorf("expected only built-in hooks (%d), got %d", len(hooks.BuiltinHooks()), len(eng.Hooks))
	}
}

// =====================================================================
// ForAgent
// =====================================================================

func TestHookSource_ForAgent_ReturnsAgentSpecificHooks(t *testing.T) {
	app := makeApp("app-1", nil, []schema.Agent{
		{ID: "main", Hooks: []schema.Hook{appHook("main_hook", schema.HookEventToolStart)}},
		{ID: "worker", Hooks: []schema.Hook{appHook("worker_hook", schema.HookEventToolStart)}},
	})
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{"app-1": app}},
		hooks.ActionDeps{})

	mainHooks := src.ForAgent("app-1", "main")
	if len(mainHooks) != 1 || mainHooks[0].ID != "main_hook" {
		t.Errorf("main agent hooks : %v", mainHooks)
	}
	workerHooks := src.ForAgent("app-1", "worker")
	if len(workerHooks) != 1 || workerHooks[0].ID != "worker_hook" {
		t.Errorf("worker agent hooks : %v", workerHooks)
	}
}

func TestHookSource_ForAgent_NilSafe(t *testing.T) {
	var s *hookSource
	if got := s.ForAgent("app", "agent"); got != nil {
		t.Errorf("nil receiver should return nil hooks")
	}
	app := makeApp("app-1", nil, []schema.Agent{{ID: "main"}})
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{"app-1": app}},
		hooks.ActionDeps{})
	if got := src.ForAgent("", "main"); got != nil {
		t.Errorf("empty appID returned %v", got)
	}
	if got := src.ForAgent("app-1", ""); got != nil {
		t.Errorf("empty agentID returned %v", got)
	}
	if got := src.ForAgent("app-1", "missing"); got != nil {
		t.Errorf("missing agent returned %v", got)
	}
	if got := src.ForAgent("missing-app", "main"); got != nil {
		t.Errorf("missing app returned %v", got)
	}
}

// =====================================================================
// dispatchCaller — adapter must propagate errored outcomes as errors
// =====================================================================

type stubDispatcher struct {
	out runtime.ToolOutcome
	got runtime.ToolInvocation
}

func (s *stubDispatcher) Dispatch(_ context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	s.got = call
	return s.out
}

func TestDispatchCaller_HappyPath(t *testing.T) {
	disp := &stubDispatcher{out: runtime.ToolOutcome{Status: "completed"}}
	c := dispatchCaller{d: disp}
	if _, err := c.Call(context.Background(), "filesystem.read",
		map[string]any{"path": "/x"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if disp.got.Name != "filesystem.read" {
		t.Errorf("name = %q, want filesystem.read", disp.got.Name)
	}
	if disp.got.Args["path"] != "/x" {
		t.Errorf("args = %+v", disp.got.Args)
	}
	if disp.got.CallID == "" {
		t.Errorf("CallID empty — should mark the call as hook-originated")
	}
}

func TestDispatchCaller_ErroredOutcomeBecomesError(t *testing.T) {
	disp := &stubDispatcher{out: runtime.ToolOutcome{
		Status: "errored", Error: "denied by policy",
	}}
	c := dispatchCaller{d: disp}
	_, err := c.Call(context.Background(), "shell.bash", nil)
	if err == nil {
		t.Fatal("expected error from errored outcome")
	}
	if err.Error() != "denied by policy" {
		t.Errorf("err = %q, want %q", err.Error(), "denied by policy")
	}
}

func TestDispatchCaller_NilDispatcher(t *testing.T) {
	c := dispatchCaller{d: nil}
	if _, err := c.Call(context.Background(), "x", nil); err == nil {
		t.Fatal("expected error from nil dispatcher")
	}
}

// =====================================================================
// Concurrent ForApp — sync.Map invariants under contention
// =====================================================================

func TestHookSource_ForApp_ConcurrentCallsShareInstance(t *testing.T) {
	app := makeApp("app-1", []schema.Hook{appHook("h", schema.HookEventTurnStart)}, nil)
	src := newHookSource(&stubApps{byID: map[string]*appmgr.RuntimeApp{"app-1": app}},
		hooks.ActionDeps{})

	const N = 64
	results := make([]*hooks.Engine, N)
	done := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func(i int) { results[i] = src.ForApp("app-1"); done <- struct{}{} }(i)
	}
	for i := 0; i < N; i++ {
		<-done
	}
	first := results[0]
	if first == nil {
		t.Fatal("first goroutine got nil")
	}
	for i := 1; i < N; i++ {
		if results[i] != first {
			t.Errorf("g%d got distinct engine instance — cache race", i)
		}
	}
}
