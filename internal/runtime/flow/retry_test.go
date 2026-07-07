package flow

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func TestNodeRetrySucceedsAfterTransientFailures(t *testing.T) {
	h := newHarness()
	var calls int64
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		if atomic.AddInt64(&calls, 1) < 3 {
			return ToolOutcome{Status: "errored", Error: "rate limited", Parts: textParts("boom")}
		}
		return ToolOutcome{Status: "completed", Parts: textParts("ok:" + inv.Name)}
	}
	flow := &schema.FlowConfig{
		Entry: "n1",
		Nodes: []schema.FlowNode{{
			ID: "n1", Type: "tool", Tool: "glpi.post",
			Retry: &schema.FlowRetry{MaxAttempts: 3, BackoffMs: 1},
		}},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Errorf("tool calls = %d, want 3 (2 failures + 1 success)", got)
	}
	if res.Content != "ok:glpi.post" {
		t.Errorf("content = %q, want ok:glpi.post", res.Content)
	}
	retrying := 0
	for _, e := range h.sink.byType(sessionstore.EventFlowNodeEnd) {
		if e.Flow != nil && e.Flow.Status == "retrying" {
			retrying++
		}
	}
	if retrying != 2 {
		t.Errorf("retrying events = %d, want 2", retrying)
	}
}

func TestNodeRetryExhaustsThenOnError(t *testing.T) {
	h := newHarness()
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		return ToolOutcome{Status: "errored", Error: "always down", Parts: textParts("boom")}
	}
	flow := &schema.FlowConfig{
		Entry: "n1",
		Nodes: []schema.FlowNode{
			{ID: "n1", Type: "tool", Tool: "glpi.post",
				Retry:   &schema.FlowRetry{MaxAttempts: 2, BackoffMs: 1},
				OnError: []schema.FlowErrorRoute{{Default: true, To: "fallback"}}},
			{ID: "fallback", Type: "terminal", Params: map[string]any{"output": "handled"}},
		},
	}
	res, err := h.run(t, flow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Content != "handled" {
		t.Errorf("content = %q, want handled (on_error fired after retries)", res.Content)
	}
}

func TestNodeRetryMatchGate(t *testing.T) {
	h := newHarness()
	var calls int64
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		atomic.AddInt64(&calls, 1)
		return ToolOutcome{Status: "errored", Error: "permanent 404", Parts: textParts("boom")}
	}
	flow := &schema.FlowConfig{
		Entry: "n1",
		Nodes: []schema.FlowNode{{
			ID: "n1", Type: "tool", Tool: "glpi.post",
			Retry: &schema.FlowRetry{MaxAttempts: 4, BackoffMs: 1, Match: "rate limit|timeout"},
		}},
	}
	_, _ = h.run(t, flow)
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("tool calls = %d, want 1 (404 doesn't match retry gate)", got)
	}
}

func TestOnErrorExposesErrorContext(t *testing.T) {
	h := newHarness()
	var notified string
	h.toolFn = func(_ context.Context, inv ToolInvocation) ToolOutcome {
		if inv.Name == "glpi.post" {
			return ToolOutcome{Status: "errored", Error: "GLPI 503 unavailable", Parts: textParts("boom")}
		}
		if inv.Name == "notify.human" {
			notified, _ = inv.Args["text"].(string)
			return ToolOutcome{Status: "completed", Parts: textParts("notified")}
		}
		return ToolOutcome{Status: "completed", Parts: textParts("ok")}
	}
	flow := &schema.FlowConfig{
		Entry: "post",
		Nodes: []schema.FlowNode{
			{ID: "post", Type: "tool", Tool: "glpi.post",
				OnError: []schema.FlowErrorRoute{{Default: true, To: "escalate"}}},
			{ID: "escalate", Type: "tool", Tool: "notify.human",
				Params: map[string]any{"text": "Écriture GLPI échouée sur {{error.node}}: {{error.message}}"}},
		},
	}
	if _, err := h.run(t, flow); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "Écriture GLPI échouée sur post: flow: tool \"glpi.post\": GLPI 503 unavailable"
	if notified != want {
		t.Errorf("notification = %q\nwant %q", notified, want)
	}
}
