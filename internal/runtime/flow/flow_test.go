package flow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type recordedEvent struct {
	Type sessionstore.EventType
	Flow *sessionstore.FlowPayload
}

type mockSink struct {
	mu       sync.Mutex
	events   []recordedEvent
	onAppend func(ev sessionstore.Event)
}

func (m *mockSink) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	m.mu.Lock()
	m.events = append(m.events, recordedEvent{Type: ev.Type, Flow: ev.Flow})
	n := uint64(len(m.events))
	cb := m.onAppend
	m.mu.Unlock()
	if cb != nil {
		cb(ev)
	}
	return n, nil
}

func (m *mockSink) byType(t sessionstore.EventType) []recordedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []recordedEvent
	for _, e := range m.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func (m *mockSink) ranNode(nodeID string) bool {
	for _, e := range m.byType(sessionstore.EventFlowNodeEnd) {
		if e.Flow != nil && e.Flow.NodeID == nodeID {
			return true
		}
	}
	return false
}

func newIDGen() func() string {
	var n int64
	return func() string { return fmt.Sprintf("id-%d", atomic.AddInt64(&n, 1)) }
}

type harness struct {
	sink      *mockSink
	agentFn   func(ctx context.Context, spec AgentSpec) (AgentResult, error)
	toolFn    func(ctx context.Context, inv ToolInvocation) ToolOutcome
	approvals *approval.Registry
}

func (h *harness) deps() Deps {
	return Deps{Sessions: h.sink, RunAgent: h.agentFn, RunTool: h.toolFn, Approvals: h.approvals}
}

func (h *harness) run(t *testing.T, flow *schema.FlowConfig) (*FlowResult, error) {
	t.Helper()
	return New(flow, h.deps(), newIDGen()).Run(context.Background(), flow, RunInput("app", "sess", "user", "jwt", "turn"))
}

func newHarness() *harness {
	return &harness{
		sink: &mockSink{},
		agentFn: func(_ context.Context, spec AgentSpec) (AgentResult, error) {
			return AgentResult{Status: "completed", Content: "agent:" + spec.AgentID}, nil
		},
		toolFn: func(_ context.Context, inv ToolInvocation) ToolOutcome {
			return ToolOutcome{Status: "completed", Parts: textParts("tool:" + inv.Name)}
		},
		approvals: approval.NewRegistry(),
	}
}

func textParts(s string) []sessionstore.MessagePart {
	return []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: s}}
}

func route(when, to string) schema.FlowRoute { return schema.FlowRoute{When: when, To: to} }
func dflt(to string) schema.FlowRoute        { return schema.FlowRoute{Default: true, To: to} }
func branches(ids ...string) []schema.FlowBranch {
	out := make([]schema.FlowBranch, len(ids))
	for i, id := range ids {
		out[i] = schema.FlowBranch{To: id}
	}
	return out
}

func TestRoutesChain(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "writer", Routes: []schema.FlowRoute{route("default", "b")}},
			{ID: "b", Type: "tool", Tool: "fs.read", Routes: []schema.FlowRoute{route("default", "end")}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "tool:fs.read" {
		t.Fatalf("got %q", res.Content)
	}
	if !h.sink.ranNode("a") || !h.sink.ranNode("b") {
		t.Error("expected both nodes to run")
	}
}

func TestBooleanRouting(t *testing.T) {
	cases := []struct {
		name      string
		agentJSON string
		want      string
		node      string
	}{
		{"refund", `{"category":"refund"}`, "tool:refund.process", "refund"},
		{"tech", `{"category":"tech"}`, "tool:tech.handle", "tech"},
		{"fallback", `{"category":"other"}`, "tool:queue.add", "general"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
				return AgentResult{Status: "completed", Content: tc.agentJSON}, nil
			}
			flow := &schema.FlowConfig{
				Entry: "triage",
				Nodes: []schema.FlowNode{
					{ID: "triage", Type: "agent", Agent: "bot", Routes: []schema.FlowRoute{
						route("category == 'refund'", "refund"),
						route("category == 'tech'", "tech"),
						dflt("general"),
					}},
					{ID: "refund", Type: "tool", Tool: "refund.process"},
					{ID: "tech", Type: "tool", Tool: "tech.handle"},
					{ID: "general", Type: "tool", Tool: "queue.add"},
				},
			}
			res, err := h.run(t, flow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Content != tc.want {
				t.Fatalf("got %q want %q", res.Content, tc.want)
			}
			if !h.sink.ranNode(tc.node) {
				t.Errorf("expected node %q to run", tc.node)
			}
		})
	}
}

func TestDecisionSwitch(t *testing.T) {
	cases := []struct {
		priority string
		want     string
	}{
		{"p0", "tool:page.oncall"},
		{"p1", "tool:assign.senior"},
		{"p3", "tool:queue.standard"},
	}
	for _, tc := range cases {
		t.Run(tc.priority, func(t *testing.T) {
			h := newHarness()
			h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
				return AgentResult{Status: "completed", Content: `{"ticket":{"priority":"` + tc.priority + `"}}`}, nil
			}
			flow := &schema.FlowConfig{
				Entry: "classify",
				Nodes: []schema.FlowNode{
					{ID: "classify", Type: "agent", Agent: "bot", Routes: []schema.FlowRoute{route("default", "route")}},
					{ID: "route", Type: "decision", Expr: "ticket.priority", Routes: []schema.FlowRoute{
						route("p0", "emergency"),
						route("p1", "senior"),
						dflt("standard"),
					}},
					{ID: "emergency", Type: "tool", Tool: "page.oncall"},
					{ID: "senior", Type: "tool", Tool: "assign.senior"},
					{ID: "standard", Type: "tool", Tool: "queue.standard"},
				},
			}
			res, err := h.run(t, flow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Content != tc.want {
				t.Fatalf("got %q want %q", res.Content, tc.want)
			}
		})
	}
}

func TestEndSentinel(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x", Routes: []schema.FlowRoute{route("default", "end")}},
			{ID: "never", Type: "agent", Agent: "y"},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.sink.ranNode("never") {
		t.Error("node after end sentinel should not run")
	}
}

func TestParallelJoinAll(t *testing.T) {
	h := newHarness()
	var calls int64
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		atomic.AddInt64(&calls, 1)
		return AgentResult{Status: "completed", Content: spec.AgentID}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "fan",
		Nodes: []schema.FlowNode{
			{ID: "fan", Type: "parallel", Branches: branches("x", "y", "z"),
				Join: &schema.FlowJoinConfig{Type: "all"}, Routes: []schema.FlowRoute{route("default", "end")}},
			{ID: "x", Type: "agent", Agent: "ax"},
			{ID: "y", Type: "agent", Agent: "ay"},
			{ID: "z", Type: "agent", Agent: "az"},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("expected 3 agent calls, got %d", calls)
	}
	for _, w := range []string{"ax", "ay", "az"} {
		if !strings.Contains(res.Content, w) {
			t.Errorf("combined output missing %q: %q", w, res.Content)
		}
	}
}

func TestParallelJoinAny(t *testing.T) {
	h := newHarness()
	block := make(chan struct{})
	h.agentFn = func(ctx context.Context, spec AgentSpec) (AgentResult, error) {
		if spec.AgentID == "fast" {
			return AgentResult{Status: "completed", Content: "fast-done"}, nil
		}
		select {
		case <-ctx.Done():
			return AgentResult{}, ctx.Err()
		case <-block:
			return AgentResult{Status: "completed", Content: "slow"}, nil
		}
	}
	flow := &schema.FlowConfig{
		Entry: "fan",
		Nodes: []schema.FlowNode{
			{ID: "fan", Type: "parallel", Branches: branches("s", "f"),
				Join: &schema.FlowJoinConfig{Type: "any"}, Routes: []schema.FlowRoute{route("default", "end")}},
			{ID: "s", Type: "agent", Agent: "slow"},
			{ID: "f", Type: "agent", Agent: "fast"},
		},
	}
	start := time.Now()
	res, err := h.run(t, flow)
	close(block)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("join=any should return on first win, took %s", time.Since(start))
	}
	if !strings.Contains(res.Content, "fast-done") {
		t.Errorf("expected fast output, got %q", res.Content)
	}
}

func TestParallelJoinCount(t *testing.T) {
	h := newHarness()
	block := make(chan struct{})
	h.agentFn = func(ctx context.Context, spec AgentSpec) (AgentResult, error) {
		if spec.AgentID == "a3" {
			select {
			case <-ctx.Done():
				return AgentResult{}, ctx.Err()
			case <-block:
			}
		}
		return AgentResult{Status: "completed", Content: spec.AgentID}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "fan",
		Nodes: []schema.FlowNode{
			{ID: "fan", Type: "parallel", Branches: branches("n1", "n2", "n3"),
				Join: &schema.FlowJoinConfig{Type: "count", Min: 2}, Routes: []schema.FlowRoute{route("default", "end")}},
			{ID: "n1", Type: "agent", Agent: "a1"},
			{ID: "n2", Type: "agent", Agent: "a2"},
			{ID: "n3", Type: "agent", Agent: "a3"},
		},
	}
	start := time.Now()
	_, err := h.run(t, flow)
	close(block)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("join=count:2 should not wait for blocked 3rd, took %s", time.Since(start))
	}
}

func TestErrorRouting(t *testing.T) {
	h := newHarness()
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		if inv.Name == "risky.op" {
			return ToolOutcome{Status: "errored", Error: "TimeoutError: upstream slow"}
		}
		return ToolOutcome{Status: "completed", Parts: textParts("recovered")}
	}
	flow := &schema.FlowConfig{
		Entry: "risky",
		Nodes: []schema.FlowNode{
			{ID: "risky", Type: "tool", Tool: "risky.op",
				OnError: []schema.FlowErrorRoute{{Match: "TimeoutError", To: "recover"}},
				Routes:  []schema.FlowRoute{route("default", "end")}},
			{ID: "recover", Type: "tool", Tool: "safe.op", Routes: []schema.FlowRoute{route("default", "end")}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("on_error should swallow, got %v", err)
	}
	if res.Content != "recovered" {
		t.Fatalf("got %q", res.Content)
	}
}

func TestTemplateInterpolation(t *testing.T) {
	h := newHarness()
	var seenTask string
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		if spec.AgentID == "second" {
			seenTask = spec.Task
			return AgentResult{Status: "completed", Content: "done"}, nil
		}
		return AgentResult{Status: "completed", Content: "RESULT_ONE"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "one",
		Nodes: []schema.FlowNode{
			{ID: "one", Type: "agent", Agent: "first", Routes: []schema.FlowRoute{route("default", "two")}},
			{ID: "two", Type: "agent", Agent: "second",
				Params: map[string]any{"task": "prev was: {{one.output}}"}},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenTask != "prev was: RESULT_ONE" {
		t.Fatalf("template not interpolated: %q", seenTask)
	}
}

func TestEventContext(t *testing.T) {
	h := newHarness()
	var seenTask string
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		seenTask = spec.Task
		return AgentResult{Status: "completed", Content: "ok"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "bot", Params: map[string]any{"task": "msg: {{event.payload.message}}"}},
		},
	}
	in := RunInput("app", "sess", "user", "jwt", "turn").
		WithEvent(map[string]any{"payload": map[string]any{"message": "hello world"}})
	if _, err := New(flow, h.deps(), newIDGen()).Run(context.Background(), flow, in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenTask != "msg: hello world" {
		t.Fatalf("event context not resolved: %q", seenTask)
	}
}

func TestApprovalMultiChoice(t *testing.T) {
	cases := []struct {
		name   string
		result approval.Result
		reason string
		want   string
		node   string
	}{
		{"approve", approval.ResultApproved, "", "tool:ship.it", "deploy"},
		{"escalate", approval.ResultApproved, "escalate", "tool:human.review", "review"},
		{"deny", approval.ResultDenied, "", "tool:rollback", "abort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.sink.onAppend = func(ev sessionstore.Event) {
				if ev.Type == sessionstore.EventApprovalRequest && ev.Approval != nil {
					h.approvals.Resolve(ev.Approval.ID, approval.Resolution{Result: tc.result, Reason: tc.reason})
				}
			}
			flow := &schema.FlowConfig{
				Entry: "gate",
				Nodes: []schema.FlowNode{
					{ID: "gate", Type: "approval", Message: "approve deploy?",
						Choices: []any{"approve", "reject", "escalate"},
						Routes: []schema.FlowRoute{
							route("approvals.gate == 'approve'", "deploy"),
							route("approvals.gate == 'escalate'", "review"),
							dflt("abort"),
						}},
					{ID: "deploy", Type: "tool", Tool: "ship.it"},
					{ID: "review", Type: "tool", Tool: "human.review"},
					{ID: "abort", Type: "tool", Tool: "rollback"},
				},
			}
			res, err := h.run(t, flow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Content != tc.want {
				t.Fatalf("got %q want %q", res.Content, tc.want)
			}
			if !h.sink.ranNode(tc.node) {
				t.Errorf("expected node %q", tc.node)
			}
		})
	}
}

func TestMaxIterationsGuard(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Entry:         "loop",
		MaxIterations: 5,
		Nodes: []schema.FlowNode{
			{ID: "loop", Type: "decision", Expr: "'x'", Routes: []schema.FlowRoute{dflt("loop")}},
		},
	}
	_, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("max_iterations should end gracefully per doc, got error: %v", err)
	}
	var capped bool
	for _, e := range h.sink.byType(sessionstore.EventFlowNodeEnd) {
		if e.Flow != nil && e.Flow.Status == "max_iterations" {
			capped = true
		}
	}
	if !capped {
		t.Fatal("expected a max_iterations event to be logged")
	}
	ended := h.sink.byType(sessionstore.EventFlowEnded)
	if len(ended) != 1 || ended[0].Flow.Status != "completed" {
		t.Fatalf("expected one completed flow_ended, got %+v", ended)
	}
}

func TestContextCancellation(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	h.agentFn = func(c context.Context, spec AgentSpec) (AgentResult, error) {
		cancel()
		<-c.Done()
		return AgentResult{}, c.Err()
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "slow", Routes: []schema.FlowRoute{route("default", "b")}},
			{ID: "b", Type: "agent", Agent: "never"},
		},
	}
	_, err := New(flow, h.deps(), newIDGen()).Run(ctx, flow, RunInput("app", "sess", "user", "jwt", "turn"))
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestUnknownNodeType(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{Entry: "bad", Nodes: []schema.FlowNode{{ID: "bad", Type: "warp"}}}
	_, err := h.run(t, flow)
	if err == nil || !strings.Contains(err.Error(), "unknown node type") {
		t.Fatalf("expected unknown-node-type error, got %v", err)
	}
}

func TestTerminalNode(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x", Routes: []schema.FlowRoute{route("default", "stop")}},
			{ID: "stop", Type: "terminal", Params: map[string]any{"output": "final-value"}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "final-value" {
		t.Fatalf("terminal output: got %q", res.Content)
	}
}

func TestLifecycleEvents(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{{ID: "a", Type: "agent", Agent: "x", Routes: []schema.FlowRoute{route("default", "end")}}},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h.sink.byType(sessionstore.EventFlowStarted)) != 1 {
		t.Error("missing flow_started")
	}
	if len(h.sink.byType(sessionstore.EventFlowNodeStart)) != 1 {
		t.Error("missing flow_node_started")
	}
	if len(h.sink.byType(sessionstore.EventFlowNodeEnd)) != 1 {
		t.Error("missing flow_node_ended")
	}
	ended := h.sink.byType(sessionstore.EventFlowEnded)
	if len(ended) != 1 || ended[0].Flow.Status != "completed" {
		t.Errorf("expected one completed flow_ended, got %+v", ended)
	}
}

func TestDeadEndPath(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x", Routes: []schema.FlowRoute{route("category == 'never'", "b")}},
			{ID: "b", Type: "agent", Agent: "y"},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.sink.ranNode("b") {
		t.Error("no route should have matched; b should not run")
	}
}
