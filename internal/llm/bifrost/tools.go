package bifrost

import (
	"encoding/json"
	"sort"
	"strings"

	schemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/mbathepaul/digitorn/internal/llm"
)

// Tool wire-up between llm.* and Bifrost's schemas.*. Kept in its own file
// so the hot path stays auditable and the conversion functions are easy to
// benchmark. Allocations are pre-sized for the 100K-concurrent-session
// target (each chat round allocates exactly the slices it needs, no growth).

// buildBifrostTools converts a digitorn tool catalog into the Bifrost
// schemas.ChatTool shape expected by every provider Bifrost talks to.
// Returns nil for an empty input so we never attach an empty tools array
// (some providers refuse it).
func buildBifrostTools(specs []llm.ToolSpec) []schemas.ChatTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]schemas.ChatTool, 0, len(specs))
	for i := range specs {
		s := &specs[i]
		fn := &schemas.ChatToolFunction{
			Name:       s.Name,
			Parameters: convertJSONSchema(s.Parameters),
		}
		if s.Description != "" {
			desc := s.Description
			fn.Description = &desc
		}
		tool := schemas.ChatTool{
			Type:     schemas.ChatToolTypeFunction,
			Function: fn,
		}
		// Propagate Anthropic-style cache hint. Bifrost reads this
		// directly off the tool entry — no transformation needed.
		if s.CacheControl != nil {
			tool.CacheControl = toBifrostCacheControl(s.CacheControl)
		}
		out = append(out, tool)
	}
	return out
}

// convertJSONSchema turns the planner's {type, properties, required} map
// into Bifrost's typed ToolFunctionParameters. Properties is REQUIRED non-
// nil per Bifrost docs (chatcompletions.go:541) — even for zero-arg tools
// we ship an empty OrderedMap.
func convertJSONSchema(raw map[string]any) *schemas.ToolFunctionParameters {
	if raw == nil {
		return &schemas.ToolFunctionParameters{
			Type:       "object",
			Properties: schemas.NewOrderedMap(),
		}
	}
	typ, _ := raw["type"].(string)
	if typ == "" {
		typ = "object"
	}
	out := &schemas.ToolFunctionParameters{Type: typ}

	if d, ok := raw["description"].(string); ok && d != "" {
		out.Description = &d
	}

	props := schemas.NewOrderedMap()
	if rawProps, ok := raw["properties"].(map[string]any); ok && len(rawProps) > 0 {
		// Sort keys for deterministic wire format — same tool ⇒ same JSON
		// across every daemon process. This is what lets prompt-caching
		// providers (Anthropic) hit the cache across restarts.
		keys := make([]string, 0, len(rawProps))
		for k := range rawProps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		props = schemas.NewOrderedMapWithCapacity(len(keys))
		for _, k := range keys {
			props.Set(k, rawProps[k])
		}
	}
	out.Properties = props

	switch r := raw["required"].(type) {
	case []string:
		if len(r) > 0 {
			cp := make([]string, len(r))
			copy(cp, r)
			out.Required = cp
		}
	case []any:
		if len(r) > 0 {
			req := make([]string, 0, len(r))
			for _, v := range r {
				if s, ok := v.(string); ok {
					req = append(req, s)
				}
			}
			if len(req) > 0 {
				out.Required = req
			}
		}
	}

	return out
}

// toolChoiceAuto returns a *ChatToolChoice set to the "auto" string form,
// matching the OpenAI/Anthropic default behaviour : "use a tool if you
// need one, otherwise reply with text".
func toolChoiceAuto() *schemas.ChatToolChoice {
	s := string(schemas.ChatToolChoiceTypeAuto)
	return &schemas.ChatToolChoice{ChatToolChoiceStr: &s}
}

// buildAssistantToolCalls converts our llm.ChatToolCall list (as kept in
// prior assistant messages) into Bifrost's ChatAssistantMessageToolCall
// list — needed so multi-round tool-calling tells the provider "here's
// what you already called and here are the results".
//
// Arguments is round-tripped as JSON because Bifrost's wire format is a
// raw JSON string (matching OpenAI), not a structured map.
func buildAssistantToolCalls(calls []llm.ChatToolCall) []schemas.ChatAssistantMessageToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]schemas.ChatAssistantMessageToolCall, 0, len(calls))
	for i := range calls {
		c := &calls[i]
		fnName := c.Name
		fn := schemas.ChatAssistantMessageToolCallFunction{
			Name:      &fnName,
			Arguments: encodeArgs(c.Arguments),
		}
		typeStr := "function"
		if c.Type != "" {
			typeStr = c.Type
		}
		id := c.ID
		out = append(out, schemas.ChatAssistantMessageToolCall{
			Index:    uint16(i),
			Type:     &typeStr,
			ID:       &id,
			Function: fn,
		})
	}
	return out
}

// encodeArgs serialises a tool argument map to the stringified JSON form
// Bifrost (and OpenAI) expect. Returns "{}" for nil/empty so providers
// always see a syntactically valid object.
func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// decodeArgs parses Bifrost's stringified-JSON arguments back into the
// map shape the runtime dispatcher consumes. Empty / malformed string
// yields nil — the dispatcher treats nil args as "tool called with no
// arguments", which is what every spec also encodes that way.
func decodeArgs(s string) map[string]any {
	if s == "" || s == "{}" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return out
	}
	// Some models emit a bare JSON array as a tool's arguments (notably for
	// list-shaped tools like run_parallel). Unmarshalling that into a map fails ;
	// rather than dropping the call, preserve the array under a sentinel key so a
	// liberal tool parser can recover it.
	var arr []any
	if err := json.Unmarshal([]byte(s), &arr); err == nil {
		return map[string]any{llm.ArgsArrayKey: arr}
	}
	return nil
}

// extractAssistantToolCalls reads tool_calls back out of a Bifrost
// assistant message. Mirror of buildAssistantToolCalls in the opposite
// direction.
func extractAssistantToolCalls(am *schemas.ChatAssistantMessage) []llm.ChatToolCall {
	if am == nil || len(am.ToolCalls) == 0 {
		return nil
	}
	out := make([]llm.ChatToolCall, 0, len(am.ToolCalls))
	for i := range am.ToolCalls {
		tc := &am.ToolCalls[i]
		call := llm.ChatToolCall{Arguments: decodeArgs(tc.Function.Arguments)}
		if tc.ID != nil {
			call.ID = *tc.ID
		}
		if tc.Type != nil {
			call.Type = *tc.Type
		} else {
			call.Type = "function"
		}
		if tc.Function.Name != nil {
			call.Name = *tc.Function.Name
		}
		out = append(out, call)
	}
	return out
}

// rawDeltaToolCalls returns the raw, un-merged tool_call fragments
// carried by one streaming chunk. Each fragment has an Index and a
// PARTIAL Arguments JSON string — providers split a single tool call's
// arguments across many chunks (OpenAI sends the name+id in the first
// fragment, then argument-string slices keyed only by index). Callers
// MUST accumulate by index (see toolCallAccumulator) before decoding ;
// decoding a lone fragment yields nothing.
func rawDeltaToolCalls(c *schemas.BifrostStreamChunk) []schemas.ChatAssistantMessageToolCall {
	if c == nil || c.BifrostChatResponse == nil {
		return nil
	}
	cr := c.BifrostChatResponse
	if len(cr.Choices) == 0 {
		return nil
	}
	ch := cr.Choices[0]
	if ch.ChatStreamResponseChoice == nil || ch.ChatStreamResponseChoice.Delta == nil {
		return nil
	}
	return ch.ChatStreamResponseChoice.Delta.ToolCalls
}

// toolCallDeltaInfos extracts the streaming tool-call fragments of one chunk
// into the lightweight per-fragment shape forwarded to clients : index, id,
// name (set on whichever fragment carries it) and the byte length of THIS
// fragment's argument slice (the engine accumulates it into a live token
// estimate). Returns nil when the chunk carries no tool-call fragments. This is
// what makes a streaming tool call visible BEFORE it is complete.
func toolCallDeltaInfos(c *schemas.BifrostStreamChunk) []llm.ChatToolCallDelta {
	raw := rawDeltaToolCalls(c)
	if len(raw) == 0 {
		return nil
	}
	out := make([]llm.ChatToolCallDelta, 0, len(raw))
	for i := range raw {
		d := &raw[i]
		info := llm.ChatToolCallDelta{
			Index:     int(d.Index),
			ArgsChars: len(d.Function.Arguments),
		}
		if d.ID != nil {
			info.ID = *d.ID
		}
		if d.Function.Name != nil {
			info.Name = *d.Function.Name
		}
		out = append(out, info)
	}
	return out
}

// toolCallAccumulator merges streamed tool_call fragments by index into
// complete calls. This is THE fix for the streaming tool-call bug :
// without it, each fragment becomes a separate, useless tool_call with
// an empty name and un-decodable args. Insertion order is preserved so
// the merged slice matches the provider's call order.
type toolCallAccumulator struct {
	order []uint16
	byIdx map[uint16]*accToolCall
}

type accToolCall struct {
	id   string
	name string
	typ  string
	args strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIdx: map[uint16]*accToolCall{}}
}

// add folds one chunk's fragments into the accumulator. Name / ID /
// Type are taken from whichever fragment carries them (usually the
// first for a given index) ; Arguments are concatenated in arrival
// order.
func (a *toolCallAccumulator) add(deltas []schemas.ChatAssistantMessageToolCall) {
	for i := range deltas {
		d := &deltas[i]
		cur, ok := a.byIdx[d.Index]
		if !ok {
			cur = &accToolCall{}
			a.byIdx[d.Index] = cur
			a.order = append(a.order, d.Index)
		}
		if d.ID != nil && *d.ID != "" {
			cur.id = *d.ID
		}
		if d.Type != nil && *d.Type != "" {
			cur.typ = *d.Type
		}
		if d.Function.Name != nil && *d.Function.Name != "" {
			cur.name = *d.Function.Name
		}
		cur.args.WriteString(d.Function.Arguments)
	}
}

// merged returns the consolidated tool calls with their arguments
// decoded once, after all fragments have been concatenated. Empty when
// no tool_call fragments were ever seen.
func (a *toolCallAccumulator) merged() []llm.ChatToolCall {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]llm.ChatToolCall, 0, len(a.order))
	for _, idx := range a.order {
		c := a.byIdx[idx]
		typ := c.typ
		if typ == "" {
			typ = "function"
		}
		out = append(out, llm.ChatToolCall{
			ID:        c.id,
			Type:      typ,
			Name:      c.name,
			Arguments: decodeArgs(c.args.String()),
		})
	}
	return out
}
