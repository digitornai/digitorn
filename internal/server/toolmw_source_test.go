package server

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/core/servicebus"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	fsmod "github.com/digitornai/digitorn/internal/modules/filesystem"
	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/pkg/module"
)

type fakeGetter struct{ app *appmgr.RuntimeApp }

func (g fakeGetter) Get(context.Context, string) (*appmgr.RuntimeApp, error) { return g.app, nil }

// countingBus wraps a real ServiceBus and counts how many module calls reach
// the terminal — i.e. how many the onion actually let through.
type countingBus struct {
	inner ports.ServiceBus
	calls atomic.Int64
}

func (c *countingBus) Register(m domainmodule.Module) error      { return c.inner.Register(m) }
func (c *countingBus) Unregister(id string) error                { return c.inner.Unregister(id) }
func (c *countingBus) Get(id string) (domainmodule.Module, bool) { return c.inner.Get(id) }
func (c *countingBus) List() []domainmodule.Module               { return c.inner.List() }
func (c *countingBus) Call(ctx context.Context, moduleID, toolName string, params []byte) (tool.Result, error) {
	c.calls.Add(1)
	return c.inner.Call(ctx, moduleID, toolName, params)
}

// TestToolPipelineSource_RealStackOnion proves the PRODUCTION wiring end to
// end : the real toolPipelineSource resolves the onion from a real app's
// tools.modules.filesystem.middleware config, the real BusAdapter runs it
// around a real filesystem module, audit logs every call, and dedup collapses
// a repeat within a session while a different session runs its own call.
func TestToolPipelineSource_RealStackOnion(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fsmod.New()
	if err := fs.Init(context.Background(), map[string]any{"workspace": workspace}); err != nil {
		t.Fatalf("filesystem init: %v", err)
	}
	inner := servicebus.New()
	if err := inner.Register(fs); err != nil {
		t.Fatalf("register fs: %v", err)
	}
	bus := &countingBus{inner: inner}

	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "app1", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "app1"},
			Tools: &schema.ToolsBlock{
				Modules: map[string]schema.ModuleBlock{
					"filesystem": {Middleware: []map[string]any{
						{"audit": map[string]any{}},
						{"dedup": map[string]any{"window_seconds": 60.0}},
					}},
				},
			},
		},
	}

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	src := newToolPipelineSource(fakeGetter{app: app}, nil, nil, logger)
	a := &dispatch.BusAdapter{Bus: bus, Pipelines: src}

	call := runtime.ToolInvocation{
		Name: "filesystem.read", Args: map[string]any{"path": "f.txt"},
		AppID: "app1", SessionID: "sessA", UserID: "u",
	}
	out1 := a.Dispatch(context.Background(), call)
	out2 := a.Dispatch(context.Background(), call)
	if out1.Status != "completed" || out2.Status != "completed" {
		t.Fatalf("both reads must complete, got %q / %q (%s)", out1.Status, out2.Status, out1.Error)
	}
	if got := bus.calls.Load(); got != 1 {
		t.Errorf("dedup must collapse the repeat in the same session, module hit %d times", got)
	}

	call.SessionID = "sessB"
	a.Dispatch(context.Background(), call)
	if got := bus.calls.Load(); got != 2 {
		t.Errorf("a different session must run its own call (isolation), module hit %d times", got)
	}

	if !strings.Contains(logbuf.String(), "tool_audit") {
		t.Errorf("audit middleware must have logged the call, log:\n%s", logbuf.String())
	}
}

// TestToolPipelineSource_NoMiddlewareFastPath confirms a module with no
// configured middleware resolves to a nil pipeline (the allocation-free fast
// path).
func TestToolPipelineSource_NoMiddlewareFastPath(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta:       &appmgr.App{AppID: "app1", Enabled: true},
		Definition: &schema.AppDefinition{App: schema.AppMeta{AppID: "app1"}},
	}
	src := newToolPipelineSource(fakeGetter{app: app}, nil, nil, nil)
	if p := src.PipelineFor("app1", "filesystem"); p != nil {
		t.Errorf("no middleware must yield a nil pipeline, got %v", p)
	}
}

// TestNewToolResolver_SuggestsSameModuleTools proves auto_heal's resolver,
// built over the module registry, proposes a module's OTHER tools on a failed
// call (and never the failed tool itself).
func TestNewToolResolver_SuggestsSameModuleTools(t *testing.T) {
	if newToolResolver(nil) != nil {
		t.Error("a nil registry must yield a nil resolver (auto_heal inert)")
	}

	reg := module.NewRegistry()
	if err := reg.Register(func() domainmodule.Module { return fsmod.New() }); err != nil {
		t.Fatal(err)
	}
	resolver := newToolResolver(reg)
	if resolver == nil {
		t.Fatal("expected a resolver")
	}

	sugg := resolver("filesystem", "read")
	if len(sugg) == 0 {
		t.Fatal("expected sibling-tool suggestions for filesystem.read")
	}
	siblings := map[string]bool{}
	for _, s := range sugg {
		if s.ModuleID == "filesystem" && s.ToolName == "read" {
			t.Error("resolver must not suggest the failed tool itself")
		}
		siblings[s.ToolName] = true
	}
	if !siblings["write"] && !siblings["ls"] && !siblings["grep"] {
		t.Errorf("expected at least one known filesystem sibling tool, got %v", siblings)
	}
}
