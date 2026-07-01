package runtime

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

type appLookupStub struct{ app *appmgr.RuntimeApp }

func (a appLookupStub) Get(context.Context, string) (*appmgr.RuntimeApp, error) {
	return a.app, nil
}

// TestGateSubTool_EnforcesModeAllowList proves the fix for the meta-tool mode
// bypass : GateSubTool (the chokepoint the MetaDispatcher calls for every tool
// reached via execute_tool / run_parallel / background_run) rejects a tool
// outside the active mode's allow-list, even with no PolicyEvaluator wired.
func TestGateSubTool_EnforcesModeAllowList(t *testing.T) {
	e := &Engine{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	guard := &modeGuard{
		allowed:     map[string]struct{}{"filesystem.read": {}},
		label:       "Ask",
		allowedList: "filesystem.read",
	}
	ctx := withModeGuard(context.Background(), guard)

	// A mode-blocked sub-tool reached via a meta path is rejected.
	if out := e.GateSubTool(ctx, ToolInvocation{Name: "filesystem.write", AppID: "a", SessionID: "s"}); out == nil ||
		out.Status != "errored" || !strings.Contains(out.Error, "blocked in mode") {
		t.Fatalf("filesystem.write must be blocked via the meta chokepoint in Ask mode, got %+v", out)
	}
	// The wire-form (underscored) name canonicalises and is blocked too.
	if out := e.GateSubTool(ctx, ToolInvocation{Name: "filesystem__write", AppID: "a", SessionID: "s"}); out == nil ||
		!strings.Contains(out.Error, "blocked in mode") {
		t.Fatalf("filesystem__write (wire form) must also be blocked, got %+v", out)
	}
	// An allowed sub-tool passes (no PolicyEvaluator → nil = proceed).
	if out := e.GateSubTool(ctx, ToolInvocation{Name: "filesystem.read", AppID: "a", SessionID: "s"}); out != nil {
		t.Errorf("filesystem.read is in Ask mode, must pass, got %+v", out)
	}
	// Without a mode guard in ctx, nothing is mode-blocked.
	if out := e.GateSubTool(context.Background(), ToolInvocation{Name: "filesystem.write", AppID: "a", SessionID: "s"}); out != nil {
		t.Errorf("no mode guard → no mode block, got %+v", out)
	}
}

// TestGateSubTool_EnforcesBehaviorBlock proves a behavior `action: block` rule
// also applies to tools reached via the meta chokepoint (execute_tool, …) —
// closing the same bypass class for behavior as for mode.
func TestGateSubTool_EnforcesBehaviorBlock(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "a", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "a"},
			Security: &schema.SecurityBlock{Behavior: &schema.BehaviorConfig{
				RuleDefinitions: []schema.BehaviorRuleDefinition{{
					ID: "no_write", When: schema.RuleWhenPreTool, Action: schema.RuleActionBlock,
					Trigger: []string{"filesystem.write"}, Message: "writes disabled",
				}},
			}},
		},
	}
	e := &Engine{Apps: appLookupStub{app: app}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// A behavior-blocked sub-tool via the meta path is rejected.
	if out := e.GateSubTool(context.Background(), ToolInvocation{Name: "filesystem.write", AppID: "a", SessionID: "s"}); out == nil ||
		!strings.Contains(out.Error, "[BEHAVIOR BLOCKED]") || !strings.Contains(out.Error, "no_write") {
		t.Fatalf("filesystem.write must be behavior-blocked via the meta chokepoint, got %+v", out)
	}
	// A tool with no block rule passes.
	if out := e.GateSubTool(context.Background(), ToolInvocation{Name: "filesystem.read", AppID: "a", SessionID: "s"}); out != nil {
		t.Errorf("filesystem.read has no block rule, must pass, got %+v", out)
	}
}
