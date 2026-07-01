package flow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// Route on a tool node's structured output: a tool returns JSON, the next
// route reads one of its fields via `<node>.<field>`.
func TestToolResultRouting(t *testing.T) {
	h := newHarness()
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		if inv.Name == "lint.run" {
			return ToolOutcome{Status: "completed", Parts: textParts(`{"verdict":"pass"}`)}
		}
		return ToolOutcome{Status: "completed", Parts: textParts("tool:" + inv.Name)}
	}
	flow := &schema.FlowConfig{
		Entry: "check",
		Nodes: []schema.FlowNode{
			{ID: "check", Type: "tool", Tool: "lint.run", Routes: []schema.FlowRoute{
				route("check.verdict == 'pass'", "ok"),
				dflt("fail"),
			}},
			{ID: "ok", Type: "tool", Tool: "deploy.go"},
			{ID: "fail", Type: "tool", Tool: "rollback.go"},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "tool:deploy.go" {
		t.Fatalf("expected route on tool field verdict==pass, got %q", res.Content)
	}
}

// Route on a tool node's `.result` text directly.
func TestToolResultDotResult(t *testing.T) {
	h := newHarness()
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		return ToolOutcome{Status: "completed", Parts: textParts("APPROVED")}
	}
	flow := &schema.FlowConfig{
		Entry: "t",
		Nodes: []schema.FlowNode{
			{ID: "t", Type: "tool", Tool: "x.y", Routes: []schema.FlowRoute{
				route("t.result == 'APPROVED'", "go"),
				dflt("stop"),
			}},
			{ID: "go", Type: "terminal", Params: map[string]any{"output": "went"}},
			{ID: "stop", Type: "terminal", Params: map[string]any{"output": "stopped"}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "went" {
		t.Fatalf("expected .result routing, got %q", res.Content)
	}
}

// Parallel join timeout: a branch that never finishes is cancelled when the
// timeout elapses; the join still returns with the finished branch.
func TestParallelJoinTimeout(t *testing.T) {
	h := newHarness()
	h.agentFn = func(ctx context.Context, spec AgentSpec) (AgentResult, error) {
		if spec.AgentID == "slow" {
			<-ctx.Done()
			return AgentResult{}, ctx.Err()
		}
		return AgentResult{Status: "completed", Content: "fast"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "fan",
		Nodes: []schema.FlowNode{
			{ID: "fan", Type: "parallel", Branches: branches("a", "b"),
				Join:   &schema.FlowJoinConfig{Type: "count", Min: 1, Timeout: 0.3},
				Routes: []schema.FlowRoute{dflt("done")}},
			{ID: "a", Type: "agent", Agent: "fast"},
			{ID: "b", Type: "agent", Agent: "slow"},
			{ID: "done", Type: "terminal", Params: map[string]any{"output": "joined"}},
		},
	}
	start := time.Now()
	_, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout should cancel the slow branch quickly, took %s", time.Since(start))
	}
}

// Terminal without an output param falls back to the last node's text.
func TestTerminalFallbackOutput(t *testing.T) {
	h := newHarness()
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		return AgentResult{Status: "completed", Content: "carried-forward"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x", Routes: []schema.FlowRoute{dflt("stop")}},
			{ID: "stop", Type: "terminal"},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "carried-forward" {
		t.Fatalf("terminal should fall back to last text, got %q", res.Content)
	}
}

// An agent node that errors with no on_error route fails the whole flow.
func TestAgentErrorNoRecovery(t *testing.T) {
	h := newHarness()
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		return AgentResult{Status: "errored", Error: "model refused"},
			&joinError{msg: "agent failed"}
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x", Routes: []schema.FlowRoute{dflt("b")}},
			{ID: "b", Type: "agent", Agent: "y"},
		},
	}
	_, err := h.run(t, flow)
	if err == nil {
		t.Fatal("agent error without on_error must fail the flow")
	}
	if h.sink.ranNode("b") {
		t.Error("downstream node must not run after an unrecovered error")
	}
}

// Agent input alias: `user_message` is accepted when `task` is absent.
func TestAgentInputUserMessage(t *testing.T) {
	h := newHarness()
	var seen string
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		seen = spec.Task
		return AgentResult{Status: "completed", Content: "ok"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x",
				Params: map[string]any{"user_message": "hello there"}},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != "hello there" {
		t.Fatalf("user_message alias not used, got %q", seen)
	}
}

// Agent input with no task/user_message: structured params are JSON-encoded.
func TestAgentInputJSONParams(t *testing.T) {
	h := newHarness()
	var seen string
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		seen = spec.Task
		return AgentResult{Status: "completed", Content: "ok"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x",
				Params: map[string]any{"city": "Paris", "days": "3"}},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(seen, "Paris") || !strings.Contains(seen, "city") {
		t.Fatalf("structured params should reach the agent as JSON, got %q", seen)
	}
}

// memory_seed param is forwarded to the sub-agent spec.
func TestAgentMemorySeed(t *testing.T) {
	h := newHarness()
	var seed string
	h.agentFn = func(_ context.Context, spec AgentSpec) (AgentResult, error) {
		seed = spec.MemorySeed
		return AgentResult{Status: "completed", Content: "ok"}, nil
	}
	flow := &schema.FlowConfig{
		Entry: "a",
		Nodes: []schema.FlowNode{
			{ID: "a", Type: "agent", Agent: "x",
				Params: map[string]any{"task": "go", "memory_seed": "remember: be terse"}},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed != "remember: be terse" {
		t.Fatalf("memory_seed not forwarded, got %q", seed)
	}
}

// `first` is an accepted alias for the `any` join policy.
func TestParallelJoinFirstAlias(t *testing.T) {
	h := newHarness()
	block := make(chan struct{})
	h.agentFn = func(ctx context.Context, spec AgentSpec) (AgentResult, error) {
		if spec.AgentID == "f" {
			return AgentResult{Status: "completed", Content: "first"}, nil
		}
		select {
		case <-ctx.Done():
			return AgentResult{}, ctx.Err()
		case <-block:
			return AgentResult{Status: "completed", Content: "second"}, nil
		}
	}
	flow := &schema.FlowConfig{
		Entry: "fan",
		Nodes: []schema.FlowNode{
			{ID: "fan", Type: "parallel", Branches: branches("s", "ff"),
				Join: &schema.FlowJoinConfig{Type: "first"}, Routes: []schema.FlowRoute{dflt("done")}},
			{ID: "s", Type: "agent", Agent: "slow"},
			{ID: "ff", Type: "agent", Agent: "f"},
			{ID: "done", Type: "terminal", Params: map[string]any{"output": "joined"}},
		},
	}
	start := time.Now()
	_, err := h.run(t, flow)
	close(block)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("first alias should behave like any, took %s", time.Since(start))
	}
}

// on_error regex `match` selects the matching recovery route among several.
func TestErrorRouteRegexSelection(t *testing.T) {
	h := newHarness()
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		return ToolOutcome{Status: "errored", Error: "AuthError: token expired"}
	}
	flow := &schema.FlowConfig{
		Entry: "call",
		Nodes: []schema.FlowNode{
			{ID: "call", Type: "tool", Tool: "api.get", OnError: []schema.FlowErrorRoute{
				{Match: "Timeout", To: "retry"},
				{Match: "AuthError", To: "reauth"},
				{Default: true, To: "give_up"},
			}},
			{ID: "retry", Type: "terminal", Params: map[string]any{"output": "retried"}},
			{ID: "reauth", Type: "terminal", Params: map[string]any{"output": "reauthed"}},
			{ID: "give_up", Type: "terminal", Params: map[string]any{"output": "gave up"}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "reauthed" {
		t.Fatalf("expected AuthError to route to reauth, got %q", res.Content)
	}
}

// Entry defaults to the first node when not specified.
func TestEntryDefaultsToFirstNode(t *testing.T) {
	h := newHarness()
	flow := &schema.FlowConfig{
		Nodes: []schema.FlowNode{
			{ID: "first", Type: "terminal", Params: map[string]any{"output": "from-first"}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "from-first" {
		t.Fatalf("entry should default to first node, got %q", res.Content)
	}
}
