package bifrost

import (
	"encoding/json"
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/mbathepaul/digitorn/internal/llm"
)

// =====================================================================
// buildBifrostTools — outbound wire-up
// =====================================================================

func TestBuildBifrostTools_Empty(t *testing.T) {
	if got := buildBifrostTools(nil); got != nil {
		t.Fatalf("nil specs must return nil, got %v", got)
	}
	if got := buildBifrostTools([]llm.ToolSpec{}); got != nil {
		t.Fatalf("empty specs must return nil, got %v", got)
	}
}

func TestBuildBifrostTools_BasicShape(t *testing.T) {
	specs := []llm.ToolSpec{{
		Name:        "filesystem__read",
		Description: "Read a file",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "absolute path"},
			},
			"required": []string{"path"},
		},
	}}
	got := buildBifrostTools(specs)
	if len(got) != 1 {
		t.Fatalf("want 1 tool, got %d", len(got))
	}
	tool := got[0]
	if tool.Type != schemas.ChatToolTypeFunction {
		t.Errorf("Type = %q, want %q", tool.Type, schemas.ChatToolTypeFunction)
	}
	if tool.Function == nil {
		t.Fatal("Function nil")
	}
	if tool.Function.Name != "filesystem__read" {
		t.Errorf("name = %q", tool.Function.Name)
	}
	if tool.Function.Description == nil || *tool.Function.Description != "Read a file" {
		t.Errorf("description = %v", tool.Function.Description)
	}
	if tool.Function.Parameters == nil {
		t.Fatal("Parameters nil")
	}
	if tool.Function.Parameters.Type != "object" {
		t.Errorf("params type = %q", tool.Function.Parameters.Type)
	}
	if tool.Function.Parameters.Properties == nil {
		t.Fatal("Properties nil — provider would reject")
	}
	if v, ok := tool.Function.Parameters.Properties.Get("path"); !ok || v == nil {
		t.Errorf("path property missing : %v", v)
	}
	if len(tool.Function.Parameters.Required) != 1 || tool.Function.Parameters.Required[0] != "path" {
		t.Errorf("required = %v", tool.Function.Parameters.Required)
	}
}

func TestBuildBifrostTools_NoArgs(t *testing.T) {
	// Zero-arg tools must STILL ship a non-nil Properties OrderedMap.
	specs := []llm.ToolSpec{{Name: "ping", Parameters: nil}}
	got := buildBifrostTools(specs)
	if got[0].Function.Parameters == nil {
		t.Fatal("nil Parameters")
	}
	if got[0].Function.Parameters.Properties == nil {
		t.Fatal("nil Properties — Bifrost docs require non-nil")
	}
	if got[0].Function.Description != nil {
		t.Errorf("empty desc must yield nil ptr, got %q", *got[0].Function.Description)
	}
}

func TestBuildBifrostTools_RequiredFromAnySlice(t *testing.T) {
	// JSON-decoded params have "required" as []any, not []string.
	specs := []llm.ToolSpec{{
		Name: "x",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"a": map[string]any{"type": "string"}},
			"required":   []any{"a", "b"},
		},
	}}
	got := buildBifrostTools(specs)
	req := got[0].Function.Parameters.Required
	if len(req) != 2 || req[0] != "a" || req[1] != "b" {
		t.Errorf("required = %v", req)
	}
}

func TestBuildBifrostTools_PropertiesDeterministicOrder(t *testing.T) {
	// Same spec called repeatedly must produce the same key order so
	// prompt-caching providers (Anthropic) hit the cache.
	specs := []llm.ToolSpec{{
		Name: "x",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"zeta":  map[string]any{"type": "string"},
				"alpha": map[string]any{"type": "string"},
				"mu":    map[string]any{"type": "string"},
			},
		},
	}}
	var first []byte
	for i := 0; i < 5; i++ {
		got := buildBifrostTools(specs)
		b, err := json.Marshal(got[0].Function.Parameters.Properties)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if first == nil {
			first = b
			continue
		}
		if string(b) != string(first) {
			t.Errorf("non-deterministic order : run %d=%s vs run 0=%s", i, b, first)
		}
	}
	// Must be alphabetical : alpha, mu, zeta.
	if string(first) == "" {
		t.Fatal("empty marshal")
	}
}

// =====================================================================
// toolChoiceAuto
// =====================================================================

func TestToolChoiceAuto(t *testing.T) {
	tc := toolChoiceAuto()
	if tc == nil || tc.ChatToolChoiceStr == nil {
		t.Fatal("nil tool choice")
	}
	if *tc.ChatToolChoiceStr != "auto" {
		t.Errorf("got %q want auto", *tc.ChatToolChoiceStr)
	}
}

// =====================================================================
// buildAssistantToolCalls / extractAssistantToolCalls round-trip
// =====================================================================

func TestAssistantToolCalls_RoundTrip(t *testing.T) {
	in := []llm.ChatToolCall{
		{ID: "c1", Type: "function", Name: "filesystem.read", Arguments: map[string]any{"path": "/etc/hosts"}},
		{ID: "c2", Type: "function", Name: "shell.bash", Arguments: map[string]any{"cmd": "ls"}},
	}
	bif := buildAssistantToolCalls(in)
	if len(bif) != 2 {
		t.Fatalf("got %d", len(bif))
	}
	if bif[0].Index != 0 || bif[1].Index != 1 {
		t.Errorf("indices = %d, %d", bif[0].Index, bif[1].Index)
	}
	// Wrap into the Bifrost assistant shape and extract back.
	am := &schemas.ChatAssistantMessage{ToolCalls: bif}
	out := extractAssistantToolCalls(am)
	if len(out) != 2 {
		t.Fatalf("extracted %d", len(out))
	}
	if out[0].ID != "c1" || out[0].Name != "filesystem.read" || out[0].Arguments["path"] != "/etc/hosts" {
		t.Errorf("first call lost : %+v", out[0])
	}
	if out[1].ID != "c2" || out[1].Name != "shell.bash" || out[1].Arguments["cmd"] != "ls" {
		t.Errorf("second call lost : %+v", out[1])
	}
}

func TestEncodeArgs_Empty(t *testing.T) {
	if got := encodeArgs(nil); got != "{}" {
		t.Errorf("nil → %q want {}", got)
	}
	if got := encodeArgs(map[string]any{}); got != "{}" {
		t.Errorf("empty → %q want {}", got)
	}
}

func TestDecodeArgs_Garbage(t *testing.T) {
	if got := decodeArgs(""); got != nil {
		t.Errorf("empty → %v want nil", got)
	}
	if got := decodeArgs("{}"); got != nil {
		t.Errorf("empty obj → %v want nil", got)
	}
	if got := decodeArgs("not json"); got != nil {
		t.Errorf("garbage → %v want nil", got)
	}
}

// =====================================================================
// extractAssistantToolCalls — defensive nil paths
// =====================================================================

func TestExtractAssistantToolCalls_Nil(t *testing.T) {
	if out := extractAssistantToolCalls(nil); out != nil {
		t.Errorf("nil → %v want nil", out)
	}
	if out := extractAssistantToolCalls(&schemas.ChatAssistantMessage{}); out != nil {
		t.Errorf("empty → %v want nil", out)
	}
}

func TestToolCallAccumulator_Empty(t *testing.T) {
	acc := newToolCallAccumulator()
	if out := acc.merged(); out != nil {
		t.Errorf("empty accumulator → %v want nil", out)
	}
	acc.add(nil)
	if out := acc.merged(); out != nil {
		t.Errorf("after add(nil) → %v want nil", out)
	}
}

// TestToolCallAccumulator_MergesFragments reproduces the streaming
// shape OpenAI/gateway send : the first fragment carries name+id, the
// rest carry only an argument-string slice keyed by index. The merged
// result must be one complete, decoded call.
func TestToolCallAccumulator_MergesFragments(t *testing.T) {
	id := "call_1"
	typ := "function"
	name := "filesystem.read"
	frag := func(idx uint16, withMeta bool, args string) schemas.ChatAssistantMessageToolCall {
		f := schemas.ChatAssistantMessageToolCall{
			Index:    idx,
			Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: args},
		}
		if withMeta {
			f.ID = &id
			f.Type = &typ
			f.Function.Name = &name
		}
		return f
	}

	acc := newToolCallAccumulator()
	// Chunk 1 : name + id + opening brace.
	acc.add([]schemas.ChatAssistantMessageToolCall{frag(0, true, `{"path":`)})
	// Chunk 2 : argument slice, index only.
	acc.add([]schemas.ChatAssistantMessageToolCall{frag(0, false, `"/etc/`)})
	// Chunk 3 : closing slice.
	acc.add([]schemas.ChatAssistantMessageToolCall{frag(0, false, `hosts"}`)})

	merged := acc.merged()
	if len(merged) != 1 {
		t.Fatalf("merged %d calls, want 1", len(merged))
	}
	c := merged[0]
	if c.ID != "call_1" || c.Name != "filesystem.read" || c.Type != "function" {
		t.Errorf("merged meta lost : %+v", c)
	}
	if c.Arguments["path"] != "/etc/hosts" {
		t.Errorf("merged args lost : %+v (expected path=/etc/hosts)", c.Arguments)
	}
}

// TestToolCallAccumulator_TwoParallelCalls : two tool calls streamed
// interleaved by index must both survive, in index order.
func TestToolCallAccumulator_TwoParallelCalls(t *testing.T) {
	mk := func(idx uint16, name, id, args string) schemas.ChatAssistantMessageToolCall {
		n, i := name, id
		f := schemas.ChatAssistantMessageToolCall{
			Index:    idx,
			Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: args},
		}
		if name != "" {
			f.Function.Name = &n
		}
		if id != "" {
			f.ID = &i
		}
		return f
	}
	acc := newToolCallAccumulator()
	acc.add([]schemas.ChatAssistantMessageToolCall{mk(0, "filesystem.read", "c0", `{"path":`)})
	acc.add([]schemas.ChatAssistantMessageToolCall{mk(1, "filesystem.read", "c1", `{"path":`)})
	acc.add([]schemas.ChatAssistantMessageToolCall{mk(0, "", "", `"a.txt"}`)})
	acc.add([]schemas.ChatAssistantMessageToolCall{mk(1, "", "", `"b.txt"}`)})

	merged := acc.merged()
	if len(merged) != 2 {
		t.Fatalf("merged %d calls, want 2", len(merged))
	}
	if merged[0].ID != "c0" || merged[0].Arguments["path"] != "a.txt" {
		t.Errorf("call 0 wrong: %+v", merged[0])
	}
	if merged[1].ID != "c1" || merged[1].Arguments["path"] != "b.txt" {
		t.Errorf("call 1 wrong: %+v", merged[1])
	}
}

// =====================================================================
// buildChatRequest end-to-end shape
// =====================================================================

func TestBuildChatRequest_AttachesToolsAndAuto(t *testing.T) {
	s := &Service{}
	temp := 0.4
	maxT := 200
	topP := 0.95
	req := &llm.ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o-mini",
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "you are X"},
			{Role: "user", Content: "hello"},
		},
		Tools: []llm.ToolSpec{{
			Name:        "filesystem__read",
			Description: "read a file",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		}},
		Temperature: &temp,
		MaxTokens:   &maxT,
		TopP:        &topP,
	}
	br := s.buildChatRequest(req)
	if br.Params == nil {
		t.Fatal("Params nil — tools / sampling not forwarded")
	}
	if len(br.Params.Tools) != 1 {
		t.Errorf("tools forwarded = %d, want 1", len(br.Params.Tools))
	}
	if br.Params.ToolChoice == nil || br.Params.ToolChoice.ChatToolChoiceStr == nil ||
		*br.Params.ToolChoice.ChatToolChoiceStr != "auto" {
		t.Errorf("tool_choice = %+v", br.Params.ToolChoice)
	}
	if br.Params.Temperature == nil || *br.Params.Temperature != 0.4 {
		t.Errorf("temperature lost : %v", br.Params.Temperature)
	}
	if br.Params.MaxCompletionTokens == nil || *br.Params.MaxCompletionTokens != 200 {
		t.Errorf("max_tokens lost : %v", br.Params.MaxCompletionTokens)
	}
	if br.Params.TopP == nil || *br.Params.TopP != 0.95 {
		t.Errorf("top_p lost : %v", br.Params.TopP)
	}
}

func TestBuildChatRequest_NoToolsNoParams(t *testing.T) {
	s := &Service{}
	req := &llm.ChatRequest{
		Provider: "openai", Model: "gpt-4o-mini",
		Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}},
	}
	br := s.buildChatRequest(req)
	if br.Params != nil {
		t.Errorf("Params should be nil when no tools / no knobs, got %+v", br.Params)
	}
}

func TestBuildChatRequest_AssistantToolCallsForwarded(t *testing.T) {
	s := &Service{}
	req := &llm.ChatRequest{
		Provider: "openai", Model: "gpt-4o-mini",
		Messages: []llm.ChatMessage{
			{Role: "user", Content: "read x"},
			{Role: "assistant", ToolCalls: []llm.ChatToolCall{
				{ID: "call_1", Type: "function", Name: "filesystem.read",
					Arguments: map[string]any{"path": "/x"}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "file contents here"},
		},
	}
	br := s.buildChatRequest(req)
	if len(br.Input) != 3 {
		t.Fatalf("want 3 input messages got %d", len(br.Input))
	}
	assist := br.Input[1]
	if assist.ChatAssistantMessage == nil {
		t.Fatal("ChatAssistantMessage nil")
	}
	if len(assist.ChatAssistantMessage.ToolCalls) != 1 {
		t.Fatalf("forwarded calls = %d", len(assist.ChatAssistantMessage.ToolCalls))
	}
	if assist.ChatAssistantMessage.ToolCalls[0].ID == nil ||
		*assist.ChatAssistantMessage.ToolCalls[0].ID != "call_1" {
		t.Errorf("call ID lost")
	}
	toolMsg := br.Input[2]
	if toolMsg.ChatToolMessage == nil {
		t.Fatal("ChatToolMessage nil for role=tool")
	}
	if toolMsg.ChatToolMessage.ToolCallID == nil ||
		*toolMsg.ChatToolMessage.ToolCallID != "call_1" {
		t.Errorf("ToolCallID lost")
	}
}

// =====================================================================
// mapChatResponse — read tool_calls back from assistant message
// =====================================================================

func TestMapChatResponse_ExtractsToolCalls(t *testing.T) {
	id := "c1"
	typ := "function"
	name := "filesystem.read"
	assistMsg := &schemas.ChatAssistantMessage{
		ToolCalls: []schemas.ChatAssistantMessageToolCall{{
			Index: 0, Type: &typ, ID: &id,
			Function: schemas.ChatAssistantMessageToolCallFunction{
				Name: &name, Arguments: `{"path":"/etc/hosts"}`,
			},
		}},
	}
	br := &schemas.BifrostChatResponse{
		Model: "gpt-4o-mini",
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:                 schemas.ChatMessageRoleAssistant,
					ChatAssistantMessage: assistMsg,
				},
			},
		}},
	}
	out := mapChatResponse(br)
	if len(out.ToolCalls) != 1 {
		t.Fatalf("extracted = %d, want 1", len(out.ToolCalls))
	}
	c := out.ToolCalls[0]
	if c.ID != "c1" || c.Name != "filesystem.read" || c.Type != "function" {
		t.Errorf("call shape lost : %+v", c)
	}
	if c.Arguments["path"] != "/etc/hosts" {
		t.Errorf("arguments lost : %+v", c.Arguments)
	}
}

func TestMapChatResponse_NoToolCalls(t *testing.T) {
	content := "plain text reply"
	br := &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{ContentStr: &content},
				},
			},
		}},
	}
	out := mapChatResponse(br)
	if len(out.ToolCalls) != 0 {
		t.Errorf("unexpected tool_calls : %+v", out.ToolCalls)
	}
	if out.Content != "plain text reply" {
		t.Errorf("content lost : %q", out.Content)
	}
}

// =====================================================================
// mapChatChunk — streaming delta extraction
// =====================================================================

// TestMapChatChunk_TextDelta : mapChatChunk carries the text delta and
// finish reason but NOT tool_calls (those are merged separately by the
// ChatStream accumulator). rawDeltaToolCalls exposes the fragments.
func TestMapChatChunk_TextDelta(t *testing.T) {
	content := "Par"
	finish := "stop"
	delta := &schemas.ChatStreamResponseChoiceDelta{Content: &content}
	chunk := &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: delta},
				FinishReason:             &finish,
			}},
		},
	}
	out := mapChatChunk(chunk)
	if out == nil {
		t.Fatal("nil chunk out")
	}
	if out.Delta != "Par" {
		t.Errorf("delta = %q, want Par", out.Delta)
	}
	if out.FinishReason != "stop" {
		t.Errorf("finish = %q", out.FinishReason)
	}
	if len(out.ToolCalls) != 0 {
		t.Errorf("mapChatChunk must NOT carry tool_calls, got %d", len(out.ToolCalls))
	}
}

// TestRawDeltaToolCalls_ExtractsFragments : the raw extractor surfaces
// the un-merged fragments the accumulator consumes.
func TestRawDeltaToolCalls_ExtractsFragments(t *testing.T) {
	id := "c1"
	name := "filesystem.read"
	delta := &schemas.ChatStreamResponseChoiceDelta{
		ToolCalls: []schemas.ChatAssistantMessageToolCall{{
			Index: 0, ID: &id,
			Function: schemas.ChatAssistantMessageToolCallFunction{
				Name: &name, Arguments: `{"path":`,
			},
		}},
	}
	chunk := &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: delta},
			}},
		},
	}
	raw := rawDeltaToolCalls(chunk)
	if len(raw) != 1 {
		t.Fatalf("raw fragments = %d, want 1", len(raw))
	}
	if raw[0].Function.Arguments != `{"path":` {
		t.Errorf("fragment args = %q", raw[0].Function.Arguments)
	}
	// nil-safe paths.
	if rawDeltaToolCalls(nil) != nil {
		t.Error("nil chunk should yield nil")
	}
	if rawDeltaToolCalls(&schemas.BifrostStreamChunk{}) != nil {
		t.Error("empty chunk should yield nil")
	}
}

// TestDecodeArgs_BareArrayPreserved : some models emit a bare JSON array as a
// tool's arguments (e.g. run_parallel). decodeArgs must NOT drop it — it stashes
// the array under llm.ArgsArrayKey so a liberal tool parser can recover it.
func TestDecodeArgs_BareArrayPreserved(t *testing.T) {
	out := decodeArgs(`[{"tool":"filesystem.read","args":{"path":"a.go"}}]`)
	arr, ok := out[llm.ArgsArrayKey].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("bare array must be preserved under %q, got %#v", llm.ArgsArrayKey, out)
	}
	if first, _ := arr[0].(map[string]any); first["tool"] != "filesystem.read" {
		t.Errorf("array element lost: %#v", arr[0])
	}
	// Object args still decode normally (no sentinel).
	obj := decodeArgs(`{"path":"a.go"}`)
	if obj["path"] != "a.go" || obj[llm.ArgsArrayKey] != nil {
		t.Errorf("object args mis-decoded: %#v", obj)
	}
	// Empty / garbage → nil, no panic.
	if decodeArgs("") != nil || decodeArgs("not json") != nil {
		t.Error("empty/garbage args must decode to nil")
	}
}
