package bifrost

import (
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
)

// TestMarkStablePrefix_NoBreakpointsWhenNothingStable: an empty
// conversation gets no markers. Guards against false-positive cache hints
// on degenerate inputs.
func TestMarkStablePrefix_NoBreakpointsWhenNothingStable(t *testing.T) {
	cases := []struct {
		name string
		req  *llm.ChatRequest
	}{
		{"nil", nil},
		{"empty", &llm.ChatRequest{}},
		{"single user, no tools, no system", &llm.ChatRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := markStablePrefixCacheable(c.req); got != 0 {
				t.Errorf("got %d markers, want 0", got)
			}
		})
	}
}

// TestMarkStablePrefix_SystemOnly: a request with only a system + user
// message gets exactly 1 marker (on the system).
func TestMarkStablePrefix_SystemOnly(t *testing.T) {
	req := &llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "You are a careful assistant."},
			{Role: "user", Content: "Hello!"},
		},
	}
	got := markStablePrefixCacheable(req)
	if got != 1 {
		t.Errorf("got %d markers, want 1", got)
	}
	if req.Messages[0].CacheControl == nil {
		t.Error("system msg not marked")
	}
	if req.Messages[0].CacheControl.Type != "ephemeral" {
		t.Errorf("marker type = %q, want ephemeral", req.Messages[0].CacheControl.Type)
	}
	if req.Messages[1].CacheControl != nil {
		t.Error("user msg was marked, should not be")
	}
}

// TestMarkStablePrefix_SystemAndTools: system + tools → 2 markers.
func TestMarkStablePrefix_SystemAndTools(t *testing.T) {
	req := &llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "You are an agent."},
			{Role: "user", Content: "Read foo.txt."},
		},
		Tools: []llm.ToolSpec{
			{Name: "read_file"},
			{Name: "write_file"},
		},
	}
	got := markStablePrefixCacheable(req)
	if got != 2 {
		t.Errorf("got %d markers, want 2", got)
	}
	if req.Messages[0].CacheControl == nil {
		t.Error("system not marked")
	}
	if req.Tools[1].CacheControl == nil {
		t.Error("last tool not marked")
	}
	if req.Tools[0].CacheControl != nil {
		t.Error("first tool unexpectedly marked")
	}
}

// TestMarkStablePrefix_LongHistoryAddsPreRecentBreakpoint: a conversation
// long enough to have stable history gets a 3rd marker before the recent
// 2-message tail.
func TestMarkStablePrefix_LongHistoryAddsPreRecentBreakpoint(t *testing.T) {
	req := &llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "u2"},
			{Role: "assistant", Content: "a2"},
			{Role: "user", Content: "u3"}, // ← anchor expected here (len-3)
			{Role: "assistant", Content: "a3"},
			{Role: "user", Content: "current question"},
		},
	}
	got := markStablePrefixCacheable(req)
	if got != 2 {
		t.Errorf("got %d markers, want 2 (system + pre-recent)", got)
	}
	// System gets #1
	if req.Messages[0].CacheControl == nil {
		t.Error("system not marked")
	}
	// Pre-recent anchor (= len - recentTurnsKeptUncached - 1 = 5) gets #2 (or 3 if tools)
	expectedAnchor := len(req.Messages) - 1 - recentTurnsKeptUncached
	if req.Messages[expectedAnchor].CacheControl == nil {
		t.Errorf("expected anchor at index %d not marked", expectedAnchor)
	}
}

// TestMarkStablePrefix_VeryLongHistoryAddsDeepAnchor: above
// deepAnchorThreshold messages, breakpoint #4 kicks in.
func TestMarkStablePrefix_VeryLongHistoryAddsDeepAnchor(t *testing.T) {
	msgs := []llm.ChatMessage{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ { // 12 history msgs = 13 total > deepAnchorThreshold
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, llm.ChatMessage{Role: role, Content: "m"})
	}
	req := &llm.ChatRequest{Messages: msgs}
	got := markStablePrefixCacheable(req)
	// system (#1) + pre-recent (#3) + deep (#4) = 3 markers (no tools)
	if got != 3 {
		t.Errorf("got %d markers, want 3", got)
	}
}

// TestMarkStablePrefix_NeverExceedsFour: even with system + tools + long
// history, we cap at 4 breakpoints (Anthropic's hard limit).
func TestMarkStablePrefix_NeverExceedsFour(t *testing.T) {
	msgs := []llm.ChatMessage{{Role: "system", Content: "sys"}}
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, llm.ChatMessage{Role: role, Content: "m"})
	}
	req := &llm.ChatRequest{
		Messages: msgs,
		Tools:    []llm.ToolSpec{{Name: "t1"}, {Name: "t2"}},
	}
	got := markStablePrefixCacheable(req)
	if got > 4 {
		t.Errorf("got %d markers, want at most 4 (Anthropic hard cap)", got)
	}
}

// TestMarkStablePrefix_NoOverlap: never put two markers on the same
// message — overlapping breakpoints are wasted.
func TestMarkStablePrefix_NoOverlap(t *testing.T) {
	req := &llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "current"},
		},
	}
	got := markStablePrefixCacheable(req)
	if got > 2 {
		t.Errorf("got %d markers — overlap likely on short convo", got)
	}
	marked := 0
	for i := range req.Messages {
		if req.Messages[i].CacheControl != nil {
			marked++
		}
	}
	if marked != got {
		t.Errorf("marker accounting mismatch: counter=%d, actual marks=%d", got, marked)
	}
}

// TestMarkStablePrefix_Idempotent: running twice produces the same result.
func TestMarkStablePrefix_Idempotent(t *testing.T) {
	build := func() *llm.ChatRequest {
		return &llm.ChatRequest{
			Messages: []llm.ChatMessage{
				{Role: "system", Content: "sys"},
				{Role: "user", Content: "u1"},
				{Role: "assistant", Content: "a1"},
				{Role: "user", Content: "u2"},
				{Role: "assistant", Content: "a2"},
				{Role: "user", Content: "current"},
			},
			Tools: []llm.ToolSpec{{Name: "tool1"}},
		}
	}
	req1 := build()
	first := markStablePrefixCacheable(req1)
	second := markStablePrefixCacheable(req1)
	if first != second {
		t.Errorf("not idempotent: first=%d, second=%d", first, second)
	}
}

// TestMarkMessageCacheable_PrefersLastBlock: when Parts are present, the
// marker goes on the last block, not the message-level field.
func TestMarkMessageCacheable_PrefersLastBlock(t *testing.T) {
	m := &llm.ChatMessage{
		Role: "system",
		Parts: []llm.ContentPart{
			{Type: "text", Text: "block 0"},
			{Type: "text", Text: "block 1 — final"},
		},
	}
	markMessageCacheable(m)
	if m.CacheControl != nil {
		t.Error("message-level marker set when blocks present")
	}
	if m.Parts[0].CacheControl != nil {
		t.Error("first block marked — should be only the last")
	}
	if m.Parts[1].CacheControl == nil {
		t.Error("last block not marked")
	}
}

// TestNeedsContentBlocks_TogglesCorrectly: needsContentBlocks returns
// true exactly when ContentBlocks form is required (any cache hint or
// any Part present).
func TestNeedsContentBlocks_TogglesCorrectly(t *testing.T) {
	cases := []struct {
		name string
		m    *llm.ChatMessage
		want bool
	}{
		{"nil → false", nil, false},
		{"empty → false", &llm.ChatMessage{Role: "user"}, false},
		{"content string only → false", &llm.ChatMessage{Role: "user", Content: "hi"}, false},
		{"message cache hint → true", &llm.ChatMessage{Role: "system", Content: "sys", CacheControl: ephemeralCC}, true},
		{"any part → true", &llm.ChatMessage{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "x"}}}, true},
		{"part with cache → true", &llm.ChatMessage{Role: "system", Parts: []llm.ContentPart{{Type: "text", Text: "x", CacheControl: ephemeralCC}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsContentBlocks(c.m); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
