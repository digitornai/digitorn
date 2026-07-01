package runtime_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// RT-2 tests. Verify the chain "agent's declared modules → tool catalog
// → ChatRequest.Tools → ChatResponse.ToolCalls → durable EventToolCall
// events + assistant_message multipart parts".

// TestTools_NoCatalog_NoToolsField : without a wired catalog the runtime
// must NOT inject anything into ChatRequest.Tools. Single-turn chat with
// no tools is the V0 path that existed before RT-2 — it has to keep
// working bit-for-bit.
func TestTools_NoCatalog_NoToolsField(t *testing.T) {
	apps := &stubApps{app: okApp(t, "you are helpful", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := okLLM()

	e := newEngine(t, apps, sess, lc)
	// e.Tools left nil — falls back to NoToolsCatalog
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	if len(lc.got.Tools) != 0 {
		t.Fatalf("nil catalog should yield empty Tools, got %+v", lc.got.Tools)
	}
}

// TestTools_CatalogInjects_AllTools : a configured catalog returning
// 2 tool specs must show up verbatim in ChatRequest.Tools, in order.
func TestTools_CatalogInjects_AllTools(t *testing.T) {
	tools := []llm.ToolSpec{
		{Name: "web_search", Description: "search the web", Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"q": map[string]any{"type": "string"},
			},
			"required": []string{"q"},
		}},
		{Name: "calculator", Description: "do arithmetic"},
	}
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := okLLM()

	e := newEngine(t, apps, sess, lc)
	e.Tools = &runtime.StaticToolCatalog{Tools: tools}
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(lc.got.Tools) != 2 {
		t.Fatalf("expected 2 tools in ChatRequest, got %d : %+v", len(lc.got.Tools), lc.got.Tools)
	}
	if lc.got.Tools[0].Name != "web_search" || lc.got.Tools[1].Name != "calculator" {
		t.Fatalf("tool order broken : %+v", lc.got.Tools)
	}
}

// TestTools_SingleToolCall_EmitsEvent : when the LLM responds with one
// tool_call, the runtime persists exactly one EventToolCall event AND
// the assistant message carries a corresponding ToolCall part. The
// downstream RT-3 dispatcher relies on BOTH paths being durable.
func TestTools_SingleToolCall_EmitsEvent(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{
		resp: &llm.ChatResponse{
			Content: "Let me search that for you.",
			ToolCalls: []llm.ChatToolCall{
				{ID: "call-1", Type: "function", Name: "web_search", Arguments: map[string]any{"q": "weather paris"}},
			},
			Model: "gpt-4o-mini",
		},
	}

	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := sess.countAppend(sessionstore.EventToolCall); got != 1 {
		t.Fatalf("expected 1 EventToolCall, got %d", got)
	}
	tc := sess.findAppend(sessionstore.EventToolCall)
	if tc.Tool == nil || tc.Tool.CallID != "call-1" || tc.Tool.Name != "web_search" {
		t.Fatalf("EventToolCall payload wrong : %+v", tc)
	}
	if tc.Tool.Status != "pending" {
		t.Fatalf("EventToolCall status should be 'pending', got %q", tc.Tool.Status)
	}
	if v, ok := tc.Tool.Arguments["q"].(string); !ok || v != "weather paris" {
		t.Fatalf("EventToolCall args lost : %+v", tc.Tool.Arguments)
	}

	// The assistant_message must carry the tool_call as a Part — so
	// projection + LLM-adapter can reconstruct the same shape on the
	// next turn without re-querying the EventToolCall.
	asst := sess.findAppend(sessionstore.EventAssistantMessage)
	if asst == nil || asst.Message == nil {
		t.Fatal("no assistant_message persisted")
	}
	if len(asst.Message.Parts) != 2 {
		t.Fatalf("assistant message should have 2 parts (text + tool_call), got %d : %+v",
			len(asst.Message.Parts), asst.Message.Parts)
	}
	if asst.Message.Parts[0].Type != sessionstore.PartTypeText {
		t.Fatalf("first part should be text, got %q", asst.Message.Parts[0].Type)
	}
	if asst.Message.Parts[1].Type != sessionstore.PartTypeToolCall {
		t.Fatalf("second part should be tool_call, got %q", asst.Message.Parts[1].Type)
	}
	if asst.Message.Parts[1].ToolCall.ID != "call-1" {
		t.Fatalf("tool_call ID lost in parts : %+v", asst.Message.Parts[1].ToolCall)
	}
}

// TestTools_MultipleToolCalls_OneEventEach : parallel tool calls from
// the model must yield one EventToolCall per call (so the timeline
// shows each invocation discretely + RT-3 can dispatch them in
// parallel) and the assistant message keeps them ALL as parts.
func TestTools_MultipleToolCalls_OneEventEach(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{
		resp: &llm.ChatResponse{
			ToolCalls: []llm.ChatToolCall{
				{ID: "call-a", Type: "function", Name: "web_search", Arguments: map[string]any{"q": "weather"}},
				{ID: "call-b", Type: "function", Name: "calculator", Arguments: map[string]any{"expr": "2+2"}},
				{ID: "call-c", Type: "function", Name: "translate", Arguments: map[string]any{"text": "hi", "to": "fr"}},
			},
		},
	}

	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := sess.countAppend(sessionstore.EventToolCall); got != 3 {
		t.Fatalf("expected 3 EventToolCall events, got %d", got)
	}
	asst := sess.findAppend(sessionstore.EventAssistantMessage)
	if asst == nil || len(asst.Message.Parts) != 3 {
		// 3 tool_call parts ; no text because the LLM had no text content.
		t.Fatalf("assistant message should have 3 tool_call parts, got %d : %+v",
			len(asst.Message.Parts), asst.Message.Parts)
	}
}

// TestTools_TextOnlyResponse_NoToolCallEvents : when the LLM responds
// without tool_calls, the runtime path is identical to V0 — single
// assistant_message, zero EventToolCall events.
func TestTools_TextOnlyResponse_NoToolCallEvents(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := okLLM() // returns Content="hello back", no tool_calls

	e := newEngine(t, apps, sess, lc)
	e.Tools = &runtime.StaticToolCatalog{Tools: []llm.ToolSpec{{Name: "x"}}}
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := sess.countAppend(sessionstore.EventToolCall); got != 0 {
		t.Fatalf("expected 0 EventToolCall events on text-only response, got %d", got)
	}
	asst := sess.findAppend(sessionstore.EventAssistantMessage)
	if asst == nil || asst.Message == nil {
		t.Fatal("no assistant_message")
	}
	// Single text part, no tool calls.
	if len(asst.Message.Parts) != 1 || asst.Message.Parts[0].Type != sessionstore.PartTypeText {
		t.Fatalf("expected single text part, got %+v", asst.Message.Parts)
	}
	if asst.Message.Content != "hello back" {
		t.Fatalf("legacy Content wrong : %q", asst.Message.Content)
	}
}

// TestTools_NoToolsCatalog_Type : verify the exported NoToolsCatalog
// type is reachable by daemon wire-up code and returns nil for any
// agent — defensive smoke test against future refactors.
func TestTools_NoToolsCatalog_Type(t *testing.T) {
	var c runtime.ToolCatalog = runtime.NoToolsCatalog{}
	if got := c.ToolsForAgent(&schema.Agent{ID: "any"}); got != nil {
		t.Fatalf("NoToolsCatalog must return nil, got %+v", got)
	}
}
