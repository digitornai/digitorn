//go:build live

package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/policy/approval"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// Live flow-engine tests. Real LLM agents classify input; the flow
// engine routes deterministically on their structured output. Each
// test proves a specific condition / route / node type fires correctly
// end-to-end against a live model.
//
//   go test -tags live ./internal/runtime/ -run TestLiveFlow
// =====================================================================

func flowBrain(f *liveEngineFixture) schema.Brain {
	return f.app.Definition.Agents[0].Brain
}

// installFlow swaps the fixture app to a flow app with the given agents+flow,
// reusing the live Brain for any agent that doesn't set one.
func installFlow(f *liveEngineFixture, agents []schema.Agent, flow *schema.FlowConfig) {
	brain := flowBrain(f)
	for i := range agents {
		if agents[i].Brain.Model == "" {
			agents[i].Brain = brain
		}
	}
	f.app.Definition.Agents = agents
	f.app.Definition.Flow = flow
}

func runFlowLive(t *testing.T, f *liveEngineFixture, userMessage string) string {
	t.Helper()
	if f.engine.ApprovalRegistry == nil {
		f.engine.ApprovalRegistry = approval.NewRegistry()
	}
	f.injectUser(t, userMessage)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := f.engine.Run(ctx, dgruntime.TurnInput{
		AppID: "live-app", SessionID: "live-sess", UserID: "test-user", UserJWT: f.userJWT,
	})
	if err != nil {
		t.Fatalf("flow run: %v", err)
	}
	if res == nil {
		return ""
	}
	return res.Content
}

func flowNodeRan(f *liveEngineFixture, nodeID string) bool {
	f.session.mu.Lock()
	defer f.session.mu.Unlock()
	for _, ev := range f.session.events {
		if ev.Type == sessionstore.EventFlowNodeEnd && ev.Flow != nil &&
			ev.Flow.NodeID == nodeID && ev.Flow.Status == "completed" {
			return true
		}
	}
	return false
}

func branches(ids ...string) []schema.FlowBranch {
	out := make([]schema.FlowBranch, len(ids))
	for i, id := range ids {
		out[i] = schema.FlowBranch{To: id}
	}
	return out
}

func classifierAgent(id, instruction string) schema.Agent {
	return schema.Agent{
		ID:           id,
		Role:         "assistant",
		SystemPrompt: instruction + "\nRespond with ONLY a JSON object and no other text.",
	}
}

// ---------------------------------------------------------------------
// Boolean equality routing — each route fires for its matching input.
// ---------------------------------------------------------------------

func TestLiveFlow_BooleanRouting(t *testing.T) {
	cases := []struct {
		name     string
		message  string
		wantNode string
	}{
		{"refund", "I want my money back for a broken product", "refund_path"},
		{"tech", "The app keeps crashing on startup, please help debug", "tech_path"},
		{"other", "What are your business hours?", "general_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := liveSetup(t)
			installFlow(f,
				[]schema.Agent{classifierAgent("triage",
					`Classify the user request into exactly one category: "refund" (money/returns), "tech" (technical problems/bugs), or "other". Output: {"category": "<one of refund|tech|other>"}.`)},
				&schema.FlowConfig{
					ID:    "triage_flow",
					Entry: "triage_node",
					Nodes: []schema.FlowNode{
						{ID: "triage_node", Type: "agent", Agent: "triage",
							Params: map[string]any{"task": "{{event.payload.message}}"},
							Routes: []schema.FlowRoute{
								{When: "category == 'refund'", To: "refund_path"},
								{When: "category == 'tech'", To: "tech_path"},
								{Default: true, To: "general_path"},
							}},
						{ID: "refund_path", Type: "terminal", Params: map[string]any{"output": "routed:refund"}},
						{ID: "tech_path", Type: "terminal", Params: map[string]any{"output": "routed:tech"}},
						{ID: "general_path", Type: "terminal", Params: map[string]any{"output": "routed:general"}},
					},
				})

			content := runFlowLive(t, f, tc.message)
			if !flowNodeRan(f, tc.wantNode) {
				t.Errorf("expected node %q to run for %q; got content %q", tc.wantNode, tc.message, content)
				dumpFlowNodes(t, f)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Decision node — switch on the value of `expr`.
// ---------------------------------------------------------------------

func TestLiveFlow_DecisionSwitch(t *testing.T) {
	cases := []struct {
		name     string
		message  string
		wantNode string
	}{
		{"p0", "URGENT: production is completely down, all users affected", "p0_path"},
		{"p3", "Minor typo in the footer text, low priority", "default_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := liveSetup(t)
			installFlow(f,
				[]schema.Agent{classifierAgent("rank",
					`Assign a priority to the incident: "p0" (critical/outage), "p1" (high), "p2" (medium), or "p3" (low). Output: {"priority": "<p0|p1|p2|p3>"}.`)},
				&schema.FlowConfig{
					ID:    "incident_flow",
					Entry: "rank_node",
					Nodes: []schema.FlowNode{
						{ID: "rank_node", Type: "agent", Agent: "rank",
							Params: map[string]any{"task": "{{event.payload.message}}"},
							Routes: []schema.FlowRoute{{Default: true, To: "decide"}}},
						{ID: "decide", Type: "decision", Expr: "priority",
							Routes: []schema.FlowRoute{
								{When: "p0", To: "p0_path"},
								{When: "p1", To: "p1_path"},
								{Default: true, To: "default_path"},
							}},
						{ID: "p0_path", Type: "terminal", Params: map[string]any{"output": "paged"}},
						{ID: "p1_path", Type: "terminal", Params: map[string]any{"output": "senior"}},
						{ID: "default_path", Type: "terminal", Params: map[string]any{"output": "queued"}},
					},
				})

			runFlowLive(t, f, tc.message)
			if !flowNodeRan(f, tc.wantNode) {
				t.Errorf("expected node %q for %q", tc.wantNode, tc.message)
				dumpFlowNodes(t, f)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Numeric comparison route — `score > N`.
// ---------------------------------------------------------------------

func TestLiveFlow_NumericComparison(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{classifierAgent("scorer",
			`Rate the sentiment of the message from 1 (very negative) to 10 (very positive). Output: {"score": <integer 1-10>}.`)},
		&schema.FlowConfig{
			ID:    "sentiment_flow",
			Entry: "score_node",
			Nodes: []schema.FlowNode{
				{ID: "score_node", Type: "agent", Agent: "scorer",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{
						{When: "score >= 7", To: "happy_path"},
						{When: "score <= 3", To: "angry_path"},
						{Default: true, To: "neutral_path"},
					}},
				{ID: "happy_path", Type: "terminal", Params: map[string]any{"output": "positive"}},
				{ID: "angry_path", Type: "terminal", Params: map[string]any{"output": "negative"}},
				{ID: "neutral_path", Type: "terminal", Params: map[string]any{"output": "neutral"}},
			},
		})

	runFlowLive(t, f, "This is the best product I have ever used, absolutely fantastic!")
	if !flowNodeRan(f, "happy_path") {
		t.Error("expected happy_path for strongly positive sentiment")
		dumpFlowNodes(t, f)
	}
}

// ---------------------------------------------------------------------
// Compound boolean condition — `and` / `or`.
// ---------------------------------------------------------------------

func TestLiveFlow_CompoundCondition(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{classifierAgent("analyze",
			`Analyze the support message. Output: {"category": "<refund|tech|other>", "urgent": <true|false>}. urgent=true only if the user expresses time pressure or anger.`)},
		&schema.FlowConfig{
			ID:    "escalate_flow",
			Entry: "analyze_node",
			Nodes: []schema.FlowNode{
				{ID: "analyze_node", Type: "agent", Agent: "analyze",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{
						{When: "category == 'refund' and urgent == true", To: "escalate_path"},
						{Default: true, To: "normal_path"},
					}},
				{ID: "escalate_path", Type: "terminal", Params: map[string]any{"output": "escalated"}},
				{ID: "normal_path", Type: "terminal", Params: map[string]any{"output": "normal"}},
			},
		})

	runFlowLive(t, f, "I demand a refund RIGHT NOW, this is unacceptable and I need it resolved today!")
	if !flowNodeRan(f, "escalate_path") {
		t.Error("expected escalate_path for urgent refund (category==refund AND urgent==true)")
		dumpFlowNodes(t, f)
	}
}

// ---------------------------------------------------------------------
// Parallel fan-out / join, then a synthesis agent reads both branches.
// ---------------------------------------------------------------------

func TestLiveFlow_ParallelJoin(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{
			classifierAgent("pros", `List two concise pros of the subject. Output: {"pros": "<text>"}.`),
			classifierAgent("cons", `List two concise cons of the subject. Output: {"cons": "<text>"}.`),
		},
		&schema.FlowConfig{
			ID:    "debate_flow",
			Entry: "fan",
			Nodes: []schema.FlowNode{
				{ID: "fan", Type: "parallel", Branches: branches("pros_node", "cons_node"),
					Join:   &schema.FlowJoinConfig{Type: "all"},
					Routes: []schema.FlowRoute{{Default: true, To: "done"}}},
				{ID: "pros_node", Type: "agent", Agent: "pros",
					Params: map[string]any{"task": "{{event.payload.message}}"}},
				{ID: "cons_node", Type: "agent", Agent: "cons",
					Params: map[string]any{"task": "{{event.payload.message}}"}},
				{ID: "done", Type: "terminal"},
			},
		})

	runFlowLive(t, f, "Remote work for software teams")
	if !flowNodeRan(f, "pros_node") || !flowNodeRan(f, "cons_node") {
		t.Error("expected both parallel branches to run")
		dumpFlowNodes(t, f)
	}
	if !flowNodeRan(f, "done") {
		t.Error("expected join to continue to done")
	}
}

// ---------------------------------------------------------------------
// Approval node — human gate (auto-resolved) routes on the choice,
// with live agents on either side.
// ---------------------------------------------------------------------

func TestLiveFlow_ApprovalGate(t *testing.T) {
	cases := []struct {
		name     string
		result   approval.Result
		reason   string
		wantNode string
	}{
		{"approve", approval.ResultApproved, "", "approved_path"},
		{"reject", approval.ResultDenied, "", "rejected_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := liveSetup(t)
			f.engine.ApprovalRegistry = approval.NewRegistry()

			installFlow(f,
				[]schema.Agent{classifierAgent("drafter", `Acknowledge the request briefly. Output: {"ack": "<text>"}.`)},
				&schema.FlowConfig{
					ID:    "approval_flow",
					Entry: "draft_node",
					Nodes: []schema.FlowNode{
						{ID: "draft_node", Type: "agent", Agent: "drafter",
							Params: map[string]any{"task": "{{event.payload.message}}"},
							Routes: []schema.FlowRoute{{Default: true, To: "gate"}}},
						{ID: "gate", Type: "approval", Message: "Approve this action?",
							Choices: []any{"approve", "reject"},
							Routes: []schema.FlowRoute{
								{When: "approvals.gate == 'approve'", To: "approved_path"},
								{Default: true, To: "rejected_path"},
							}},
						{ID: "approved_path", Type: "terminal", Params: map[string]any{"output": "approved"}},
						{ID: "rejected_path", Type: "terminal", Params: map[string]any{"output": "rejected"}},
					},
				})

			// Auto-resolve the approval as soon as the request is armed.
			go autoResolveApproval(f, tc.result, tc.reason)

			runFlowLive(t, f, "Please process my order")
			if !flowNodeRan(f, tc.wantNode) {
				t.Errorf("expected %q after %s", tc.wantNode, tc.name)
				dumpFlowNodes(t, f)
			}
		})
	}
}

func autoResolveApproval(f *liveEngineFixture, result approval.Result, reason string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		f.session.mu.Lock()
		for _, ev := range f.session.events {
			if ev.Type == sessionstore.EventApprovalRequest && ev.Approval != nil && ev.Approval.Kind == "flow_node" {
				id = ev.Approval.ID
			}
		}
		f.session.mu.Unlock()
		if id != "" {
			if f.engine.ApprovalRegistry.Resolve(id, approval.Resolution{Result: result, Reason: reason}) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// Multi-hop path — agent → decision → agent → terminal.
// ---------------------------------------------------------------------

func TestLiveFlow_MultiHop(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{
			classifierAgent("lang", `Detect the language. Output: {"lang": "<en|fr|other>"}.`),
			classifierAgent("reply_en", `Reply in English with a one-line greeting. Output: {"reply": "<text>"}.`),
			classifierAgent("reply_fr", `Réponds en français avec une salutation d'une ligne. Output: {"reply": "<text>"}.`),
		},
		&schema.FlowConfig{
			ID:    "lang_flow",
			Entry: "detect",
			Nodes: []schema.FlowNode{
				{ID: "detect", Type: "agent", Agent: "lang",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "route"}}},
				{ID: "route", Type: "decision", Expr: "lang",
					Routes: []schema.FlowRoute{
						{When: "fr", To: "fr_node"},
						{Default: true, To: "en_node"},
					}},
				{ID: "fr_node", Type: "agent", Agent: "reply_fr",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "end"}}},
				{ID: "en_node", Type: "agent", Agent: "reply_en",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "end"}}},
			},
		})

	runFlowLive(t, f, "Bonjour, comment allez-vous aujourd'hui ?")
	if !flowNodeRan(f, "fr_node") {
		t.Error("expected French branch for French input")
		dumpFlowNodes(t, f)
	}
}

// on_error route — a real tool failure (read a missing file) routes to recovery.
func TestLiveFlow_OnErrorRoute(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{classifierAgent("noop", `Acknowledge. Output: {"ok": true}.`)},
		&schema.FlowConfig{
			ID:    "recover_flow",
			Entry: "read_missing",
			Nodes: []schema.FlowNode{
				{ID: "read_missing", Type: "tool", Tool: "filesystem.read",
					Params: map[string]any{"path": "does-not-exist-xyz.txt"},
					OnError: []schema.FlowErrorRoute{{Default: true, To: "recover"}},
					Routes:  []schema.FlowRoute{{Default: true, To: "happy"}}},
				{ID: "recover", Type: "terminal", Params: map[string]any{"output": "recovered"}},
				{ID: "happy", Type: "terminal", Params: map[string]any{"output": "ok"}},
			},
		})

	content := runFlowLive(t, f, "anything")
	if !flowNodeRan(f, "recover") {
		t.Error("expected on_error to route to recover after a tool failure")
		dumpFlowNodes(t, f)
	}
	if flowNodeRan(f, "happy") {
		t.Error("happy path must not run when the tool errored")
	}
	if content != "recovered" {
		t.Errorf("expected recovered output, got %q", content)
	}
}

func dumpFlowNodes(t *testing.T, f *liveEngineFixture) {
	t.Helper()
	f.session.mu.Lock()
	defer f.session.mu.Unlock()
	for _, ev := range f.session.events {
		if (ev.Type == sessionstore.EventFlowNodeEnd || ev.Type == sessionstore.EventFlowNodeStart) && ev.Flow != nil {
			t.Logf("  flow %s node=%s type=%s status=%s out=%q",
				ev.Type, ev.Flow.NodeID, ev.Flow.NodeType, ev.Flow.Status, truncate(ev.Flow.Output, 80))
		}
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
