package hooks

import "github.com/mbathepaul/digitorn/internal/compiler/schema"

// Payload carries the firing event's data into the condition
// evaluator and the action executor. Field semantics align with
// docs-site/language/31-tool-hooks.md "Templating in actions" :
//
//	{{tool.name}}        ← ToolName
//	{{tool.params.X}}    ← ToolArgs[X] (dotted access supported)
//	{{tool.result.X}}    ← ToolResult[X]
//	{{tool.error}}       ← ToolError
//
// The TurnState fields (TokensUsed, MaxTokens, MessageCount, ...)
// feed the `context_pressure`, `turn_count`, `message_count`,
// `tool_calls` conditions. The runtime fills what it knows at the
// fire site ; the rest stays zero and conditions degrade
// gracefully (e.g. context_pressure with MaxTokens=0 returns
// false).
type Payload struct {
	Event     schema.HookEvent
	AppID     string
	SessionID string
	UserID    string
	TurnID    string
	AgentID   string

	// Tool* — filled for tool_start (formerly pre_tool_use) and
	// tool_end (formerly post_tool_use).
	ToolName   string
	ToolArgs   map[string]any
	ToolStatus string         // "completed" | "errored" (tool_end only)
	ToolError  string         // tool_end only
	ToolResult map[string]any // tool_end only

	// LLM* — filled for content_contains (matches Content) and
	// expression conditions. Engine sets these when known.
	LLMContent  string
	UserMessage string

	// TurnState* — feed turn_count / message_count / tool_calls /
	// context_pressure conditions. Engine fills from current turn
	// snapshot.
	TurnCount     int
	MessageCount  int
	ToolCallsUsed int

	TokensUsed int
	MaxTokens  int

	// Task plan state — filled for the `stop` event so a directive
	// hook can see whether the agent is about to end its turn with
	// unfinished work. OpenTasks feeds the `open_tasks` expression
	// variable ; TasksSummary feeds the {{tasks.summary}} template
	// (e.g. "t2 (in_progress), t3 (pending)").
	OpenTasks    int
	TasksSummary string

	// ErrorType — filled for the `error` event. Matches
	// error_type condition.
	ErrorType string
}

// cloneForAsync returns a copy safe to hand to an async hook goroutine. The
// scalar fields copy by value ; the mutable maps (ToolArgs / ToolResult) get
// fresh top-level copies so a SYNC transform_params/result action mutating the
// original maps in place (p.ToolArgs[k]=v) can never race a concurrent read in
// an async hook — the Go runtime panics on a concurrent map read+write, and the
// async path previously shared the very same map header via a shallow struct
// copy. Transforms only set top-level keys, so a top-level copy is sufficient.
func (p Payload) cloneForAsync() Payload {
	if p.ToolArgs != nil {
		m := make(map[string]any, len(p.ToolArgs))
		for k, v := range p.ToolArgs {
			m[k] = v
		}
		p.ToolArgs = m
	}
	if p.ToolResult != nil {
		m := make(map[string]any, len(p.ToolResult))
		for k, v := range p.ToolResult {
			m[k] = v
		}
		p.ToolResult = m
	}
	return p
}
