package contextcompact

import (
	"fmt"
	"strings"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// MicroCompact (CTX-8 Phase 2) shrinks the model's VIEW cheaply, with NO LLM
// call and WITHOUT dropping any message: it replaces the body of STALE, BULKY
// tool results with a compact reference, keeping the tool_call/tool_result
// pairing intact (no orphan, no structure break). Pure + deterministic — same
// input always yields the same output, so the view stays reproducible.
//
// It operates on the already-compacted view (after ApplyView). The newest
// keepRecentResults tool results are left FULL — the agent almost always needs
// the most recent outputs verbatim. Older tool results whose text exceeds
// minBytes are elided; small or recent outputs are untouched. The original
// messages are never mutated — a copy is returned only when something changed.
func MicroCompact(msgs []sessionstore.Message, keepRecentResults, minBytes int) []sessionstore.Message {
	if keepRecentResults < 0 {
		keepRecentResults = 0
	}
	if minBytes <= 0 {
		minBytes = 4096
	}

	var resultIdx []int
	for i := range msgs {
		if isToolResultMessage(msgs[i]) {
			resultIdx = append(resultIdx, i)
		}
	}
	elideBefore := len(resultIdx) - keepRecentResults
	if elideBefore <= 0 {
		return msgs // nothing old enough to elide
	}

	toElide := make(map[int]struct{})
	for k := 0; k < elideBefore; k++ {
		i := resultIdx[k]
		if toolResultTextLen(msgs[i]) >= minBytes {
			toElide[i] = struct{}{}
		}
	}
	if len(toElide) == 0 {
		return msgs
	}

	out := make([]sessionstore.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if _, ok := toElide[i]; ok {
			out[i] = elideToolResult(out[i])
		}
	}
	return out
}

func isToolResultMessage(m sessionstore.Message) bool {
	if m.Role == "tool" {
		return true
	}
	for _, p := range m.Parts {
		if p.Type == sessionstore.PartTypeToolResult && p.ToolResult != nil {
			return true
		}
	}
	return false
}

// toolResultTextLen is the byte length of the tool result's textual body — the
// thing eliding actually frees. Prefers the structured Parts; falls back to
// Content (the legacy single-string mirror).
func toolResultTextLen(m sessionstore.Message) int {
	n := 0
	sawPart := false
	for _, p := range m.Parts {
		if p.Type == sessionstore.PartTypeToolResult && p.ToolResult != nil {
			for _, rp := range p.ToolResult.Parts {
				if rp.Type == sessionstore.PartTypeText {
					n += len(rp.Text)
					sawPart = true
				}
			}
		}
	}
	if !sawPart {
		return len(m.Content)
	}
	return n
}

// elideToolResult returns a COPY of a tool-result message whose body is replaced
// by a compact reference, preserving the ToolCallID + Error so the pairing and
// failure signal survive. Non-tool-result messages are returned unchanged.
func elideToolResult(m sessionstore.Message) sessionstore.Message {
	ref := fmt.Sprintf("[tool output elided to save context — %d chars. Re-run the tool if you need it again.]", toolResultTextLen(m))
	out := m
	out.Content = ref
	if len(m.Parts) > 0 {
		parts := make([]sessionstore.MessagePart, len(m.Parts))
		copy(parts, m.Parts)
		for i := range parts {
			if parts[i].Type == sessionstore.PartTypeToolResult && parts[i].ToolResult != nil {
				tr := *parts[i].ToolResult
				tr.Parts = []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: ref}}
				parts[i].ToolResult = &tr
			} else if parts[i].Type == sessionstore.PartTypeText {
				parts[i].Text = ref
			}
		}
		out.Parts = parts
	} else {
		out.Parts = []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: ref}}
	}
	return out
}

// microRefMarker is the literal an elided result starts with — exported-ish via
// a helper so tests and any client can recognise a micro-compacted result.
func isElidedRef(s string) bool {
	return strings.HasPrefix(s, "[tool output elided to save context")
}
