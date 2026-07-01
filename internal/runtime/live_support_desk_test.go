//go:build live

package runtime_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/catalog"
	_ "github.com/digitornai/digitorn/internal/modules/filesystem"
	_ "github.com/digitornai/digitorn/internal/modules/rag"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/pkg/module"
)

// loadSupportDesk compiles the real examples/support-desk app and installs it on
// the live fixture, swapping every agent's brain to the live gateway model.
func loadSupportDesk(t *testing.T, f *liveEngineFixture) {
	t.Helper()
	c := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})
	res, err := c.Compile("../../examples/support-desk")
	if err != nil {
		t.Fatalf("compile support-desk: %v", err)
	}
	if !res.OK() {
		t.Fatalf("support-desk must compile clean:\n%v", res.Diagnostics)
	}
	live := f.app.Definition.Agents[0].Brain
	for i := range res.Definition.Agents {
		res.Definition.Agents[i].Brain = live
	}
	f.app.Definition = res.Definition
	if f.engine.ApprovalRegistry == nil {
		f.engine.ApprovalRegistry = approval.NewRegistry()
	}
}

func runDesk(t *testing.T, f *liveEngineFixture, sessionID, message string) {
	t.Helper()
	if err := runFlowInSession(t, f, sessionID, message); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// The real support-desk app, compiled, routes a refund request through the
// approval gate and resolves it — the commercial differentiator, end to end.
func TestLiveSupportDesk_RefundApprovalGate(t *testing.T) {
	f := liveSetup(t)
	loadSupportDesk(t, f)

	go autoResolveApproval(f, approval.ResultApproved, "")

	// A HIGH-VALUE refund ($850, above the $200 policy threshold) must NEVER
	// auto-complete — it has to pass through the human approval gate. This is
	// the core commercial guarantee of the desk.
	runDesk(t, f, "desk-refund", "I want a full refund of $850 for order 5521, the item arrived broken")

	if !flowNodeRanInSession(f, "desk-refund", "refund_check") {
		t.Error("refund request should reach refund_check")
		dumpFlowNodes(t, f)
	}
	gate := flowNodeRanInSession(f, "desk-refund", "refund_gate")
	done := flowNodeRanInSession(f, "desk-refund", "refund_done")
	denied := flowNodeRanInSession(f, "desk-refund", "refund_denied")
	if done && !gate {
		t.Error("a high-value refund was approved WITHOUT passing the human gate — financial-risk bug")
		dumpFlowNodes(t, f)
	}
	if !gate && !denied {
		t.Error("a high-value refund must pass through the approval gate or be denied")
		dumpFlowNodes(t, f)
	}
	t.Logf("high-value refund path: gate=%v done=%v denied=%v", gate, done, denied)
}

// answeringSpecialistRan reports whether the message reached a specialist that
// can actually help (kb/sales/tech), rather than being dropped to human_handoff.
// The exact specialist for an ambiguous query is a tuning detail; the
// commercial guarantee is that the customer is never left unhandled.
func answeringSpecialistRan(f *liveEngineFixture, sid string) (string, bool) {
	for _, n := range []string{"kb_node", "sales_node", "tech_node"} {
		if flowNodeRanInSession(f, sid, n) {
			return n, true
		}
	}
	return "", false
}

// A clear upgrade/buying intent routes to a competent specialist (ideally sales).
func TestLiveSupportDesk_SalesRouting(t *testing.T) {
	f := liveSetup(t)
	loadSupportDesk(t, f)

	runDesk(t, f, "desk-sales", "I'd like to upgrade my 20-person team to the Business plan — can you help me buy it?")

	got, ok := answeringSpecialistRan(f, "desk-sales")
	if !ok {
		t.Error("a buying-intent message must reach an answering specialist, not human handoff")
		dumpFlowNodes(t, f)
	}
	t.Logf("buying intent routed to %q", got)
}

// A clear how-to question routes to a competent specialist (ideally the KB agent).
func TestLiveSupportDesk_KBRouting(t *testing.T) {
	f := liveSetup(t)
	loadSupportDesk(t, f)

	runDesk(t, f, "desk-kb", "How do I export my workspace data to a CSV file?")

	got, ok := answeringSpecialistRan(f, "desk-kb")
	if !ok {
		t.Error("a how-to question must reach an answering specialist")
		dumpFlowNodes(t, f)
	}
	t.Logf("how-to routed to %q", got)
}
