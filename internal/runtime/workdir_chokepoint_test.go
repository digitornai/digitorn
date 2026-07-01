package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

// wdNoSessions is a no-op SessionAccess so emitSecurityDecision (audit row on
// the allow path) has a sink and doesn't nil-panic in this focused test.
type wdNoSessions struct{ SessionAccess }

func (wdNoSessions) AppendDurable(context.Context, sessionstore.Event) (uint64, error) {
	return 0, nil
}

func (wdNoSessions) Append(context.Context, sessionstore.Event) (uint64, error) {
	return 0, nil
}

func wdTestEngine() *Engine {
	return &Engine{
		Sessions:        wdNoSessions{},
		PolicyEvaluator: &DefaultPolicyEvaluator{Lookup: wdLookup{}},
	}
}

// wdLookup is a minimal ToolSpecLookup exposing one tool whose `path` param is
// marked Path:true, so the chokepoint knows to confine it.
type wdLookup struct{}

func (wdLookup) LookupToolSpec(module, action string) *tool.Spec {
	if module == "filesystem" && action == "read" {
		return &tool.Spec{
			Name:      "filesystem.read",
			RiskLevel: tool.RiskLow,
			Params:    []tool.ParamSpec{{Name: "path", Type: "string", Path: true}},
		}
	}
	return nil
}

func wdActiveApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "a", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "a"},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					Grant:         []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"read"}}},
				},
			},
		},
	}
}

// TestEnforceGate_WorkdirConfinesPathArgs is the WD-2 chokepoint proof: with a
// PathPolicy on ctx, enforceGate rewrites a valid path arg to the enforced
// absolute path and lets the call proceed, and rejects an escaping path BEFORE
// the call ever reaches a module.
func TestEnforceGate_WorkdirConfinesPathArgs(t *testing.T) {
	e := wdTestEngine()
	app := wdActiveApp()
	wd := t.TempDir()
	pp := workdir.NewPolicy(workdir.Options{Root: wd, Home: t.TempDir()})
	ctx := workdir.WithPathPolicy(context.Background(), pp)

	// Valid relative path → allowed, arg rewritten in place to the abs path.
	args := map[string]any{"path": "notes/todo.txt"}
	if out := e.enforceGate(ctx, "s", "a", "u", "c", app, nil, "filesystem.read", "call-1", args, nil, nil); out != nil {
		t.Fatalf("valid path must pass the chokepoint, got %+v", out)
	}
	want := filepath.Join(pp.Root(), "notes", "todo.txt")
	if args["path"] != want {
		t.Errorf("path arg not rewritten to enforced abs: got %v want %q", args["path"], want)
	}

	// Escaping path → blocked at the chokepoint with a workdir-policy error.
	esc := map[string]any{"path": "../../../../etc/passwd"}
	out := e.enforceGate(ctx, "s", "a", "u", "c", app, nil, "filesystem.read", "call-1", esc, nil, nil)
	if out == nil || out.Status != "errored" || !strings.Contains(out.Error, "workdir") {
		t.Fatalf("escaping path must be blocked by the workdir policy, got %+v", out)
	}
}

// TestEnforceGate_NoPolicy_NoConfinement confirms the chokepoint is a no-op for
// path grounds when no workdir policy rides on ctx (non-agent callers / apps
// with no workdir).
func TestEnforceGate_NoPolicy_NoConfinement(t *testing.T) {
	e := wdTestEngine()
	app := wdActiveApp()
	args := map[string]any{"path": "../../etc/passwd"}
	out := e.enforceGate(context.Background(), "s", "a", "u", "c", app, nil, "filesystem.read", "call-1", args, nil, nil)
	if out != nil && strings.Contains(out.Error, "workdir") {
		t.Fatalf("without a policy there must be no workdir rejection, got %+v", out)
	}
}
