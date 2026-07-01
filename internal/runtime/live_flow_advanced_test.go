//go:build live

package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// runFlowInSession injects a user message into an arbitrary session and runs the
// flow there, so several flows can execute concurrently in isolated sessions.
func runFlowInSession(t *testing.T, f *liveEngineFixture, sessionID, message string) error {
	t.Helper()
	ev := sessionstore.Event{
		Type: sessionstore.EventUserMessage, SessionID: sessionID,
		AppID: "live-app", UserID: "test-user",
		Message: &sessionstore.MessagePayload{Role: "user",
			Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: message}}},
	}
	if _, err := f.session.AppendDurable(context.Background(), ev); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, err := f.engine.Run(ctx, dgruntime.TurnInput{
		AppID: "live-app", SessionID: sessionID, UserID: "test-user", UserJWT: f.userJWT,
	})
	return err
}

// flowNodeOutput returns the Output recorded on a node's flow_node_ended event,
// so a test can assert exactly what one node produced (and thus what the next
// node received through templates).
func flowNodeOutput(f *liveEngineFixture, sessionID, nodeID string) string {
	f.session.mu.Lock()
	defer f.session.mu.Unlock()
	out := ""
	for _, ev := range f.session.events {
		if ev.SessionID == sessionID && ev.Type == sessionstore.EventFlowNodeEnd &&
			ev.Flow != nil && ev.Flow.NodeID == nodeID {
			out = ev.Flow.Output
		}
	}
	return out
}

func flowNodeRanInSession(f *liveEngineFixture, sessionID, nodeID string) bool {
	f.session.mu.Lock()
	defer f.session.mu.Unlock()
	for _, ev := range f.session.events {
		if ev.SessionID == sessionID && ev.Type == sessionstore.EventFlowNodeEnd &&
			ev.Flow != nil && ev.Flow.NodeID == nodeID && ev.Flow.Status == "completed" {
			return true
		}
	}
	return false
}

func triageFlow() *schema.FlowConfig {
	return &schema.FlowConfig{
		ID:    "triage",
		Entry: "triage_node",
		Nodes: []schema.FlowNode{
			{ID: "triage_node", Type: "agent", Agent: "triage",
				Params: map[string]any{"task": "{{event.payload.message}}"},
				Routes: []schema.FlowRoute{
					{When: "category == 'refund'", To: "refund_path"},
					{When: "category == 'tech'", To: "tech_path"},
					{Default: true, To: "general_path"},
				}},
			{ID: "refund_path", Type: "terminal", Params: map[string]any{"output": "refund"}},
			{ID: "tech_path", Type: "terminal", Params: map[string]any{"output": "tech"}},
			{ID: "general_path", Type: "terminal", Params: map[string]any{"output": "general"}},
		},
	}
}

// =====================================================================
// Concurrency + session isolation : N flows run at once in separate
// sessions; each must route by its OWN input with zero cross-bleed.
// =====================================================================

func TestLiveFlowAdv_ConcurrentIsolation(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{classifierAgent("triage",
			`Classify into "refund", "tech", or "other". Output: {"category":"<refund|tech|other>"}.`)},
		triageFlow())

	type job struct {
		sid     string
		message string
		want    string
	}
	jobs := []job{
		{"sess-r1", "I want a refund for my broken order", "refund_path"},
		{"sess-t1", "The app crashes every time I open it", "tech_path"},
		{"sess-r2", "Please return my money, the item never arrived", "refund_path"},
		{"sess-t2", "I get a 500 error when saving settings", "tech_path"},
		{"sess-o1", "What time do you open on weekends?", "general_path"},
		{"sess-r3", "Refund please, wrong size shipped", "refund_path"},
	}

	var wg sync.WaitGroup
	errs := make([]error, len(jobs))
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			errs[i] = runFlowInSession(t, f, j.sid, j.message)
		}(i, j)
	}
	wg.Wait()

	for i, j := range jobs {
		if errs[i] != nil {
			t.Errorf("[%s] run error: %v", j.sid, errs[i])
			continue
		}
		if !flowNodeRanInSession(f, j.sid, j.want) {
			t.Errorf("[%s] %q expected route %q", j.sid, j.message, j.want)
		}
		// Isolation : no OTHER terminal of a different category ran in this session.
		for _, other := range []string{"refund_path", "tech_path", "general_path"} {
			if other != j.want && flowNodeRanInSession(f, j.sid, other) {
				t.Errorf("[%s] cross-bleed: unexpected node %q ran", j.sid, other)
			}
		}
	}
}

// =====================================================================
// Node-to-node data passing : node A extracts a concrete value, node B
// receives it through a template and echoes it back. We assert the exact
// value made the round-trip — proving one node's result truly reaches
// the next, not just that both ran.
// =====================================================================

func TestLiveFlowAdv_NodeToNodeData(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{
			classifierAgent("extract",
				`Extract the order number from the message. Output: {"order_id":"<the number>"}.`),
			classifierAgent("confirm",
				`You are given an order id. Echo it back verbatim. Output: {"confirmed_id":"<the id you were given>"}.`),
		},
		&schema.FlowConfig{
			ID:    "handoff",
			Entry: "extract_node",
			Nodes: []schema.FlowNode{
				{ID: "extract_node", Type: "agent", Agent: "extract",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "confirm_node"}}},
				{ID: "confirm_node", Type: "agent", Agent: "confirm",
					Params: map[string]any{"task": "The order id is {{extract_node.output.order_id}}. Confirm it."},
					Routes: []schema.FlowRoute{{Default: true, To: "end"}}},
			},
		})

	if err := runFlowInSession(t, f, "handoff-sess", "Please cancel my order number 84217, it's defective"); err != nil {
		t.Fatalf("run: %v", err)
	}

	extractOut := flowNodeOutput(f, "handoff-sess", "extract_node")
	confirmOut := flowNodeOutput(f, "handoff-sess", "confirm_node")
	t.Logf("extract_node output: %s", extractOut)
	t.Logf("confirm_node output: %s", confirmOut)

	if !strings.Contains(extractOut, "84217") {
		t.Fatalf("extract_node should have captured the order id 84217, got %q", extractOut)
	}
	if !strings.Contains(confirmOut, "84217") {
		t.Errorf("confirm_node did NOT receive the order id from extract_node — data did not thread through; got %q", confirmOut)
	}
}

// =====================================================================
// Deep chain A->B->C->D : each node enriches the record and the LAST
// node pulls fields from MULTIPLE upstream nodes (up to 3 hops back),
// proving the flow context accumulates every prior node's output and
// any node can read any predecessor.
// =====================================================================

func TestLiveFlowAdv_DeepChainEnrichment(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{
			classifierAgent("extract",
				`Extract the order number. Output: {"order_id":"<number>"}.`),
			classifierAgent("categorize",
				`Classify the request as "refund", "tech", or "other". Output: {"category":"<one>"}.`),
			classifierAgent("prioritize",
				`Assign priority "high" or "low". Refunds and crashes are high. Output: {"priority":"<high|low>"}.`),
			classifierAgent("summarize",
				`Produce a one-line summary that INCLUDES the order id, the category, and the priority you are given. Output: {"summary":"<text with all three>"}.`),
		},
		&schema.FlowConfig{
			ID:    "pipeline",
			Entry: "extract_node",
			Nodes: []schema.FlowNode{
				{ID: "extract_node", Type: "agent", Agent: "extract",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "categorize_node"}}},

				{ID: "categorize_node", Type: "agent", Agent: "categorize",
					Params: map[string]any{"task": "Request: {{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "prioritize_node"}}},

				{ID: "prioritize_node", Type: "agent", Agent: "prioritize",
					Params: map[string]any{"task": "Order {{extract_node.output.order_id}} is category {{categorize_node.output.category}}. Set priority."},
					Routes: []schema.FlowRoute{{Default: true, To: "summary_node"}}},

				{ID: "summary_node", Type: "agent", Agent: "summarize",
					Params: map[string]any{"task": "Order id: {{extract_node.output.order_id}}. Category: {{categorize_node.output.category}}. Priority: {{prioritize_node.output.priority}}. Summarize including all three."},
					Routes: []schema.FlowRoute{{Default: true, To: "end"}}},
			},
		})

	msg := "I need a refund for order 73904, the product was defective"
	if err := runFlowInSession(t, f, "chain-sess", msg); err != nil {
		t.Fatalf("run: %v", err)
	}

	extractOut := flowNodeOutput(f, "chain-sess", "extract_node")
	catOut := flowNodeOutput(f, "chain-sess", "categorize_node")
	prioOut := flowNodeOutput(f, "chain-sess", "prioritize_node")
	sumOut := flowNodeOutput(f, "chain-sess", "summary_node")
	t.Logf("A extract:    %s", extractOut)
	t.Logf("B categorize: %s", catOut)
	t.Logf("C prioritize: %s", prioOut)
	t.Logf("D summary:    %s", sumOut)

	// Each link ran and enriched.
	if !strings.Contains(extractOut, "73904") {
		t.Fatalf("A: order id not extracted: %q", extractOut)
	}
	if !strings.Contains(strings.ToLower(catOut), "refund") {
		t.Errorf("B: category not refund: %q", catOut)
	}
	if !strings.Contains(strings.ToLower(prioOut), "high") {
		t.Errorf("C: priority should be high for a refund: %q", prioOut)
	}

	// D pulled from A (3 hops back) AND B (2 hops back): the final summary must
	// carry the order id and the category — proving deep context accumulation.
	if !strings.Contains(sumOut, "73904") {
		t.Errorf("D: summary missing order id from node A (3 hops upstream): %q", sumOut)
	}
	if !strings.Contains(strings.ToLower(sumOut), "refund") {
		t.Errorf("D: summary missing category from node B (2 hops upstream): %q", sumOut)
	}
}

// =====================================================================
// Bounded loop : a decision cycles back to itself; max_iterations must
// cap it and end the flow gracefully (no error, no runaway).
// =====================================================================

func TestLiveFlowAdv_BoundedLoop(t *testing.T) {
	f := liveSetup(t)
	installFlow(f, nil, &schema.FlowConfig{
		ID:            "loop",
		Entry:         "spin",
		MaxIterations: 4,
		Nodes: []schema.FlowNode{
			{ID: "spin", Type: "decision", Expr: "'go'", MaxIters: 4,
				Routes: []schema.FlowRoute{{Default: true, To: "spin"}}},
		},
	})

	done := make(chan struct{})
	go func() {
		runFlowInSession(t, f, "loop-sess", "start")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("bounded loop did not terminate — max_iterations cap failed")
	}

	var capped bool
	f.session.mu.Lock()
	for _, ev := range f.session.events {
		if ev.SessionID == "loop-sess" && ev.Flow != nil && ev.Flow.Status == "max_iterations" {
			capped = true
		}
	}
	f.session.mu.Unlock()
	if !capped {
		t.Error("expected a max_iterations cap event")
	}
}

// =====================================================================
// Adversarial structured output : the agent is told to be chatty and
// wrap its JSON in prose + fences. Routing must still extract it.
// =====================================================================

func TestLiveFlowAdv_AdversarialJSON(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{classifierAgent("chatty",
			"You are verbose. Explain your reasoning in a sentence, THEN provide the classification "+
				"inside a ```json fenced code block as {\"category\":\"refund\"} or {\"category\":\"other\"}. "+
				"Always include prose before and after the JSON.")},
		&schema.FlowConfig{
			ID:    "adv",
			Entry: "classify",
			Nodes: []schema.FlowNode{
				{ID: "classify", Type: "agent", Agent: "chatty",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{
						{When: "category == 'refund'", To: "refund_path"},
						{Default: true, To: "other_path"},
					}},
				{ID: "refund_path", Type: "terminal", Params: map[string]any{"output": "refund"}},
				{ID: "other_path", Type: "terminal", Params: map[string]any{"output": "other"}},
			},
		})

	if err := runFlowInSession(t, f, "adv-sess", "I demand my money back immediately"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !flowNodeRanInSession(f, "adv-sess", "refund_path") {
		t.Error("routing must extract JSON even when wrapped in prose + fences")
		dumpFlowNodes(t, f)
	}
}

// =====================================================================
// Realistic multi-stage workflow : classify -> parallel gather (2
// agents) -> synthesize -> tool write -> terminal. Exercises agent,
// parallel/join, template threading, a real tool, and an end path.
// =====================================================================

func TestLiveFlowAdv_MultiStageWorkflow(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{
			classifierAgent("planner", `Acknowledge the research topic. Output: {"topic":"<the topic>"}.`),
			classifierAgent("pros", `Give one concise benefit. Output: {"pro":"<text>"}.`),
			classifierAgent("cons", `Give one concise drawback. Output: {"con":"<text>"}.`),
			classifierAgent("writer", `Summarize in one line. Output: {"summary":"<text>"}.`),
		},
		&schema.FlowConfig{
			ID:    "research",
			Entry: "plan",
			Nodes: []schema.FlowNode{
				{ID: "plan", Type: "agent", Agent: "planner",
					Params: map[string]any{"task": "{{event.payload.message}}"},
					Routes: []schema.FlowRoute{{Default: true, To: "gather"}}},
				{ID: "gather", Type: "parallel", Branches: branches("pros_node", "cons_node"),
					Join:   &schema.FlowJoinConfig{Type: "all"},
					Routes: []schema.FlowRoute{{Default: true, To: "write"}}},
				{ID: "pros_node", Type: "agent", Agent: "pros",
					Params: map[string]any{"task": "Topic: {{plan.output.topic}}"}},
				{ID: "cons_node", Type: "agent", Agent: "cons",
					Params: map[string]any{"task": "Topic: {{plan.output.topic}}"}},
				{ID: "write", Type: "tool", Tool: "filesystem.write",
					Params: map[string]any{"path": "report.txt", "content": "report generated"},
					Routes: []schema.FlowRoute{{Default: true, To: "done"}}},
				{ID: "done", Type: "terminal", Params: map[string]any{"output": "complete"}},
			},
		})

	if err := runFlowInSession(t, f, "wf-sess", "electric cars"); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, n := range []string{"plan", "pros_node", "cons_node", "write", "done"} {
		if !flowNodeRanInSession(f, "wf-sess", n) {
			t.Errorf("expected node %q to run in the workflow", n)
			dumpFlowNodes(t, f)
			break
		}
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "report.txt")); err != nil {
		t.Errorf("expected the workflow tool to write report.txt: %v", err)
	}
}

// =====================================================================
// Parallel join=any under real latency : the fast branch wins; the
// flow continues without waiting for the slow branch.
// =====================================================================

func TestLiveFlowAdv_ParallelAnyLatency(t *testing.T) {
	f := liveSetup(t)
	installFlow(f,
		[]schema.Agent{
			classifierAgent("quick", `Reply with one word. Output: {"r":"fast"}.`),
			classifierAgent("verbose", `Write a long 200-word essay, then output: {"r":"slow"}.`),
		},
		&schema.FlowConfig{
			ID:    "race",
			Entry: "fan",
			Nodes: []schema.FlowNode{
				{ID: "fan", Type: "parallel", Branches: branches("quick_node", "verbose_node"),
					Join:   &schema.FlowJoinConfig{Type: "any"},
					Routes: []schema.FlowRoute{{Default: true, To: "done"}}},
				{ID: "quick_node", Type: "agent", Agent: "quick",
					Params: map[string]any{"task": "hi"}},
				{ID: "verbose_node", Type: "agent", Agent: "verbose",
					Params: map[string]any{"task": "hi"}},
				{ID: "done", Type: "terminal", Params: map[string]any{"output": "continued"}},
			},
		})

	start := time.Now()
	if err := runFlowInSession(t, f, "race-sess", "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !flowNodeRanInSession(f, "race-sess", "done") {
		t.Error("join=any must continue after the first branch completes")
	}
	t.Logf("join=any completed in %s", time.Since(start))
}
