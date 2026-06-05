package meta_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

func tinyIndex() *index.ToolIndex {
	return index.NewBuilder().Build(true, nil, nil, []policy.AvailableAction{
		{Module: "fs", Action: "read", Spec: &tool.Spec{Description: "read a file", RiskLevel: tool.RiskLow}},
		{Module: "fs", Action: "write", Spec: &tool.Spec{Description: "write a file", RiskLevel: tool.RiskHigh}},
	})
}

// TestDispatch_InnerPanic_RecoveredAsErrored : a domain tool whose handler
// PANICS must come back as an errored outcome, NEVER crash the daemon. This is
// the universal guard protecting every foreground turn goroutine and every
// background task goroutine (they funnel through Dispatch). The test process
// surviving the call IS the proof.
func TestDispatch_InnerPanic_RecoveredAsErrored(t *testing.T) {
	d := &meta.MetaDispatcher{Inner: &panicInner{boom: "danger.boom"}}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{Name: "danger.boom"})
	if out.Status != "errored" {
		t.Fatalf("panic must surface as errored, got status=%q", out.Status)
	}
	if !strings.Contains(out.Error, "panic") {
		t.Fatalf("error must mention the recovered panic: %q", out.Error)
	}
}

// TestDispatch_SearchToolsNegativeLimit_NoPanic : a negative limit must NOT
// panic make([]T,0,-1) — the clamp turns it into a valid request. Reachable by
// any LLM (search_tools is always injected), so a crash here = a 1-call daemon
// kill.
func TestDispatch_SearchToolsNegativeLimit_NoPanic(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: func(_, _ string) *index.ToolIndex { return tinyIndex() }}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"query": "file", "limit": float64(-1)},
	})
	if out.Status != "completed" {
		t.Fatalf("negative limit must be clamped and succeed, got status=%q err=%q", out.Status, out.Error)
	}
}

// TestDispatch_SearchToolsHugeLimit_NoOOM : a giant limit must be clamped so
// fetch=limit*6 can't overflow / allocate absurdly.
func TestDispatch_SearchToolsHugeLimit_NoOOM(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: func(_, _ string) *index.ToolIndex { return tinyIndex() }}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"query": "file", "limit": float64(1e18)},
	})
	if out.Status != "completed" {
		t.Fatalf("huge limit must be clamped, got status=%q err=%q", out.Status, out.Error)
	}
}

// TestDispatch_BrowseCategoryOverflowPage_NoPanic : a huge page must NOT make
// (page-1)*pageSize overflow into a negative slice bound (a daemon-crash panic).
func TestDispatch_BrowseCategoryOverflowPage_NoPanic(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: func(_, _ string) *index.ToolIndex { return tinyIndex() }}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools", // empty query + category → browse path
		Args: map[string]any{"category": "fs", "page": float64(4.7e17)},
	})
	if out.Status != "completed" {
		t.Fatalf("overflow page must clamp to an empty page, got status=%q err=%q", out.Status, out.Error)
	}
}
