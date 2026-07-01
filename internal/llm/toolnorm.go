package llm

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/digitornai/digitorn/internal/llm/toolcall"
)

// NormalizeTextToolCalls recovers tool calls from a response whose model emitted
// them as TEXT instead of native tool_calls (DeepSeek, Hermes/Qwen, older Claude
// and other open models served without a tool-call parser). It is a no-op when
// the response already carries native tool_calls or no known format is present,
// so providers that behave correctly pay only a cheap content scan.
//
// This is the single seam between the runtime and the toolcall library : the
// engine wires it as an optional ResponseNormalizer hook, keeping all format
// knowledge out of the turn loop.
func NormalizeTextToolCalls(r *ChatResponse) {
	if r == nil || len(r.ToolCalls) > 0 || r.Content == "" {
		return
	}
	res := toolcall.Extract(r.Content)
	if !res.Matched() {
		return
	}
	calls := make([]ChatToolCall, 0, len(res.Calls))
	for _, c := range res.Calls {
		calls = append(calls, ChatToolCall{
			ID:        newToolCallID(),
			Type:      "function",
			Name:      c.Name,
			Arguments: c.Arguments,
		})
	}
	r.ToolCalls = calls
	r.Content = res.Cleaned
}

func newToolCallID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "call_text"
	}
	return "call_" + hex.EncodeToString(b[:])
}
