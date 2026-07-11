package server

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/pkg/module"
)

// =====================================================================
// fakeModule — a minimal domainmodule.Module that the registry can
// register and Manifest() correctly so registryActions sees its tools.
// =====================================================================

type fakeModule struct {
	mf domainmodule.Manifest
}

func newFakeModule(id string, tools ...tool.Spec) *fakeModule {
	return &fakeModule{mf: domainmodule.Manifest{
		ID:      id,
		Version: "1.0.0",
		Tools:   tools,
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}}
}

func (f *fakeModule) Manifest() domainmodule.Manifest            { return f.mf }
func (f *fakeModule) Init(context.Context, map[string]any) error { return nil }
func (f *fakeModule) Start(context.Context) error                { return nil }
func (f *fakeModule) Stop(context.Context) error                 { return nil }
func (f *fakeModule) Invoke(context.Context, string, []byte) (tool.Result, error) {
	return tool.Result{Success: true}, nil
}

// buildRegistry creates an isolated module.Registry with the given
// factories already registered AND started, so Manifests() reflects them.
func buildRegistry(t *testing.T, mods ...*fakeModule) *module.Registry {
	t.Helper()
	reg := module.NewRegistry()
	for _, m := range mods {
		copy := m
		if err := reg.Register(func() domainmodule.Module { return copy }); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := reg.Start(context.Background(), copy.mf.ID); err != nil {
			t.Fatalf("start %s: %v", copy.mf.ID, err)
		}
	}
	return reg
}

// =====================================================================
// fakeAppMgr is a minimal appmgr.Manager : returns a fixed RuntimeApp
// by appID, or appmgr.ErrAppNotFound for anything else.
// =====================================================================

type fakeAppMgr struct {
	apps map[string]*appmgr.RuntimeApp
}

func (f *fakeAppMgr) Install(context.Context, string, string) (*appmgr.App, error) { return nil, nil }
func (f *fakeAppMgr) Upgrade(context.Context, string, string, string) (*appmgr.App, error) {
	return nil, nil
}
func (f *fakeAppMgr) Uninstall(context.Context, string, bool) error        { return nil }
func (f *fakeAppMgr) Enable(context.Context, string) error                 { return nil }
func (f *fakeAppMgr) Disable(context.Context, string) error                { return nil }
func (f *fakeAppMgr) SetBYOK(context.Context, string, bool) error          { return nil }
func (f *fakeAppMgr) SetAppPieces(context.Context, string, []string) error { return nil }
func (f *fakeAppMgr) SetDisplayName(context.Context, string, string) error { return nil }
func (f *fakeAppMgr) Reload(context.Context, string) error                 { return nil }
func (f *fakeAppMgr) CheckUpdate(context.Context, string, string) (*appmgr.UpdateInfo, error) {
	return nil, nil
}
func (f *fakeAppMgr) List(context.Context, bool) ([]appmgr.App, error)   { return nil, nil }
func (f *fakeAppMgr) BrokenApps() []appmgr.BrokenApp                     { return nil }
func (f *fakeAppMgr) ReconcileHubApps(context.Context)                   {}
func (f *fakeAppMgr) ListDisabled(context.Context) ([]appmgr.App, error) { return nil, nil }
func (f *fakeAppMgr) GetApp(context.Context, string) (*appmgr.App, error) {
	return nil, nil
}
func (f *fakeAppMgr) Get(_ context.Context, appID string) (*appmgr.RuntimeApp, error) {
	if app, ok := f.apps[appID]; ok {
		return app, nil
	}
	return nil, appmgr.ErrAppNotFound
}
func (f *fakeAppMgr) GetManifest(_ context.Context, appID string) (*schema.AppDefinition, error) {
	if app, ok := f.apps[appID]; ok && app != nil {
		return app.Definition, nil
	}
	return nil, appmgr.ErrAppNotFound
}
func (f *fakeAppMgr) Bootstrap(context.Context) error { return nil }

// appWithModules builds a RuntimeApp whose tools.modules block
// declares the given module IDs (config maps are empty).
func appWithModules(appID string, moduleIDs ...string) *appmgr.RuntimeApp {
	mods := make(map[string]schema.ModuleBlock, len(moduleIDs))
	for _, id := range moduleIDs {
		mods[id] = schema.ModuleBlock{}
	}
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, Enabled: true},
		Definition: &schema.AppDefinition{
			App:   schema.AppMeta{AppID: appID, Name: appID, Version: "1.0"},
			Tools: &schema.ToolsBlock{Modules: mods},
		},
	}
}

// =====================================================================
// Helpers
// =====================================================================

func specsByName(specs []tool.Spec) string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}

func fqnSet(actions []policy.AvailableAction) map[string]bool {
	m := make(map[string]bool, len(actions))
	for _, a := range actions {
		if a.Spec != nil {
			m[a.Spec.Name] = true
		}
	}
	return m
}

// =====================================================================
// 1. Nil safety
// =====================================================================

func TestRegistryActions_NilRegistry(t *testing.T) {
	a := registryActions{}
	out := a.ForApp("any")
	if out != nil {
		t.Errorf("nil registry → got %v, want nil", out)
	}
}

func TestRegistryActions_EmptyRegistry(t *testing.T) {
	a := registryActions{Registry: module.NewRegistry()}
	out := a.ForApp("any")
	if len(out) != 0 {
		t.Errorf("empty registry → got %d actions, want 0", len(out))
	}
}

// =====================================================================
// 2. No app manager → full universe fallback
// =====================================================================

func TestRegistryActions_NoAppMgr_ExposesEverything(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem",
			tool.Spec{Name: "read", RiskLevel: tool.RiskLow},
			tool.Spec{Name: "write", RiskLevel: tool.RiskMedium}),
		newFakeModule("shell",
			tool.Spec{Name: "bash", RiskLevel: tool.RiskHigh}),
	)
	a := registryActions{Registry: reg}
	out := a.ForApp("anything")
	fqns := fqnSet(out)
	for _, want := range []string{"filesystem.read", "filesystem.write", "shell.bash"} {
		if !fqns[want] {
			t.Errorf("missing %q in universe %v", want, fqns)
		}
	}
}

// =====================================================================
// 3. App with declared modules → intersection
// =====================================================================

func TestRegistryActions_DeclaredSubset_FiltersOthers(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem",
			tool.Spec{Name: "read"},
			tool.Spec{Name: "write"}),
		newFakeModule("shell",
			tool.Spec{Name: "bash"}),
		newFakeModule("http",
			tool.Spec{Name: "get"}),
	)
	apps := &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{
		"fs-only-app": appWithModules("fs-only-app", "filesystem"),
	}}
	a := registryActions{Registry: reg, Apps: apps}

	got := a.ForApp("fs-only-app")
	fqns := fqnSet(got)
	if !fqns["filesystem.read"] || !fqns["filesystem.write"] {
		t.Errorf("filesystem actions missing : %v", fqns)
	}
	if fqns["shell.bash"] || fqns["http.get"] {
		t.Errorf("non-declared modules leaked : %v", fqns)
	}
}

func TestRegistryActions_DeclaredButNotInRegistry_Skipped(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem", tool.Spec{Name: "read"}),
	)
	apps := &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{
		"missing-app": appWithModules("missing-app", "filesystem", "nonexistent"),
	}}
	a := registryActions{Registry: reg, Apps: apps}

	got := a.ForApp("missing-app")
	fqns := fqnSet(got)
	if !fqns["filesystem.read"] {
		t.Errorf("declared+loaded module missing : %v", fqns)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 action (only filesystem.read), got %d : %v", len(got), fqns)
	}
}

// TestRegistryActions_AppDeclaresEmpty_GetsNothing is the regression test
// for the authorization leak : a RESOLVED app that declares zero modules
// (no tools.modules block, or an empty one) must receive an EMPTY universe,
// never the full registry. A pure-chat app must never be handed filesystem
// or shell just because it forgot to opt in. This is the anti-leak invariant.
func TestRegistryActions_AppDeclaresEmpty_GetsNothing(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem", tool.Spec{Name: "read"}),
		newFakeModule("shell", tool.Spec{Name: "bash"}),
	)
	cases := map[string]*appmgr.RuntimeApp{
		"nil-tools-block": {
			Meta: &appmgr.App{AppID: "nil-tools-block", Enabled: true},
			Definition: &schema.AppDefinition{
				App:   schema.AppMeta{AppID: "nil-tools-block"},
				Tools: nil, // no tools: block at all (the leaking case)
			},
		},
		"nil-modules-map": {
			Meta: &appmgr.App{AppID: "nil-modules-map", Enabled: true},
			Definition: &schema.AppDefinition{
				App:   schema.AppMeta{AppID: "nil-modules-map"},
				Tools: &schema.ToolsBlock{Modules: nil}, // tools: present, modules empty
			},
		},
	}
	for name, app := range cases {
		t.Run(name, func(t *testing.T) {
			apps := &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{name: app}}
			a := registryActions{Registry: reg, Apps: apps}
			got := a.ForApp(name)
			if len(got) != 0 {
				t.Errorf("resolved app with no declared modules must get ZERO actions, got %d : %v", len(got), fqnSet(got))
			}
		})
	}
}

func TestRegistryActions_AppNotFound_FallsBackToUniverse(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem", tool.Spec{Name: "read"}),
	)
	apps := &fakeAppMgr{apps: nil}
	a := registryActions{Registry: reg, Apps: apps}

	got := a.ForApp("never-installed")
	if len(got) != 1 {
		t.Errorf("missing app → expected fallback universe, got %d", len(got))
	}
}

// =====================================================================
// 4. FQN construction is correct (the dispatcher contract)
// =====================================================================

func TestRegistryActions_SpecNameIsFQN(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem",
			tool.Spec{Name: "read", Description: "read a file"}),
	)
	a := registryActions{Registry: reg}
	got := a.ForApp("any")
	if len(got) != 1 {
		t.Fatalf("expected 1 action, got %d", len(got))
	}
	if got[0].Spec.Name != "filesystem.read" {
		t.Errorf("spec.Name = %q, want filesystem.read (FQN form)", got[0].Spec.Name)
	}
	if got[0].Module != "filesystem" || got[0].Action != "read" {
		t.Errorf("action shape wrong : module=%q action=%q", got[0].Module, got[0].Action)
	}
}

func TestRegistryActions_DoesNotMutateManifest(t *testing.T) {
	mod := newFakeModule("filesystem",
		tool.Spec{Name: "read", Description: "read a file"})
	reg := buildRegistry(t, mod)
	a := registryActions{Registry: reg}
	_ = a.ForApp("any")
	// Manifest's spec.Name must still be the bare action name
	// (not rewritten to FQN).
	mfSpecs := mod.Manifest().Tools
	if specsByName(mfSpecs) != "read" {
		t.Errorf("manifest tools were mutated : %s", specsByName(mfSpecs))
	}
}

// =====================================================================
// 5. Spec fidelity (other fields preserved)
// =====================================================================

func TestRegistryActions_PreservesRiskAndIrreversibility(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("danger",
			tool.Spec{
				Name:         "wipe",
				RiskLevel:    tool.RiskHigh,
				Irreversible: true,
				Tags:         []string{"destructive"},
			}),
	)
	a := registryActions{Registry: reg}
	got := a.ForApp("any")
	if got[0].Spec.RiskLevel != tool.RiskHigh {
		t.Errorf("risk lost : %q", got[0].Spec.RiskLevel)
	}
	if !got[0].Spec.Irreversible {
		t.Errorf("irreversible lost")
	}
	if len(got[0].Spec.Tags) != 1 || got[0].Spec.Tags[0] != "destructive" {
		t.Errorf("tags lost : %v", got[0].Spec.Tags)
	}
}

// =====================================================================
// 6. Concurrency
// =====================================================================

func TestRegistryActions_Concurrent(t *testing.T) {
	reg := buildRegistry(t,
		newFakeModule("filesystem",
			tool.Spec{Name: "read"}, tool.Spec{Name: "write"}),
	)
	a := registryActions{Registry: reg}

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got := a.ForApp("any")
			if len(got) != 2 {
				t.Errorf("concurrent got %d actions, want 2", len(got))
			}
		}()
	}
	wg.Wait()
}
