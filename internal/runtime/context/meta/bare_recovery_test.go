package meta_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

// capturingInner captures the canonical Name the dispatcher forwarded inward.
type capturingInner struct{ got string }

func (r *capturingInner) Dispatch(_ context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	r.got = call.Name
	return runtime.ToolOutcome{Status: "completed"}
}

// TestDispatch_BareActionRecoveredFromIndex is the STRUCTURAL guarantee that no
// module — current OR future — is denied for a dropped module prefix. The
// MetaDispatcher is the single chokepoint every tool call transits: top-level
// calls, execute_tool, and run_parallel / background_run children (which all
// re-enter Dispatch). A weak model that emits a bare domain action ("read"
// instead of "filesystem.read") must be qualified from the per-agent index
// BEFORE routing, so the gate and the inner dispatcher always see a canonical
// FQN. The index is built from the loaded modules, so a new module is covered
// the instant its tools are in the index — with no change to this path.
func TestDispatch_BareActionRecoveredFromIndex(t *testing.T) {
	// Ceiling=high + default auto so every test tool (incl. the high-risk ones)
	// survives the schema-build gates and lands in the index — recovery is
	// correctly scoped to tools the agent ACTUALLY has.
	caps := &schema.CapabilitiesConfig{
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		DefaultPolicy: schema.CapAuto,
	}
	idx := index.NewBuilder().Build(true, caps, nil, []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "write", Spec: &tool.Spec{RiskLevel: tool.RiskHigh}},
		{Module: "shell", Action: "run", Spec: &tool.Spec{RiskLevel: tool.RiskHigh}},
		// Two modules expose "list" → ambiguous, must NOT be guessed.
		{Module: "alpha", Action: "list", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
		{Module: "beta", Action: "list", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
	})
	cases := []struct{ in, want string }{
		{"read", "filesystem.read"},             // bare → recovered from index
		{"run", "shell.run"},                    // bare → recovered (any module)
		{"filesystem.read", "filesystem.read"},  // already qualified → unchanged
		{"filesystem__write", "filesystem.write"}, // wire form → canonicalised
		{"list", "list"},                        // ambiguous → left bare (gate reports honestly)
		{"nope", "nope"},                        // unknown → left bare
	}
	for _, c := range cases {
		inner := &capturingInner{}
		d := &meta.MetaDispatcher{
			Inner:       inner,
			IndexLookup: func(_, _ string) *index.ToolIndex { return idx },
		}
		d.Dispatch(context.Background(), runtime.ToolInvocation{Name: c.in, AppID: "app", AgentID: "main"})
		if inner.got != c.want {
			t.Errorf("bare %q forwarded inward as %q, want %q", c.in, inner.got, c.want)
		}
	}
}
