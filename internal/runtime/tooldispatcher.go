package runtime

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// ToolDispatcher is what the runtime calls to execute one tool. The
// implementation lives outside this package (production = the D1
// in-process ModuleDispatcher façade, tests = a fake). Keeping the
// interface here makes the dependency direction obvious : runtime
// depends on an abstraction, not on the module registry.
//
// Concurrency contract :
//
//   - Dispatch MUST be safe under concurrent calls. The runtime fans
//     out tool_calls across goroutines and expects no shared lock to
//     serialise them.
//   - Dispatch SHOULD respect ctx cancellation promptly so a turn
//     timeout / abort doesn't leak module-side work.
//   - Each call runs in ITS OWN goroutine inside the dispatcher's
//     execution lane. A slow tool blocks ONLY its caller's wait, NEVER
//     other sessions' goroutines.
type ToolDispatcher interface {
	Dispatch(ctx context.Context, call ToolInvocation) ToolOutcome
}

// ToolInvocation is what the runtime hands to the dispatcher for one
// tool call. Includes the routing context (app + agent + user) so the
// dispatcher can apply per-tenant policy and look up the per-agent
// ToolIndex (CB-3 IndexLookup contract). AgentID is the schema
// agent id of the currently-running agent in this turn — required
// for the MetaDispatcher to resolve the correct ToolIndex when
// handling meta-tool calls.
type ToolInvocation struct {
	CallID    string
	Name      string
	Args      map[string]any
	AppID     string
	AgentID   string
	UserID    string
	SessionID string

	// AgentRunID is the distinct run id of the agent making the call (the
	// entry agent's logical id, or a sub-agent's "logical#instance"). The
	// `agent` delegation tool uses it to attribute a spawned child to its
	// parent in the agent tree. Empty for non-agent-aware callers.
	AgentRunID string

	// UserJWT is the caller's gateway bearer (gateway mode). It rides along
	// so the `agent` tool can forward it to a spawned sub-agent's isolated
	// turn — a sub-agent runs on an independent context and would otherwise
	// have no credential to reach the gateway. Transient, never persisted.
	UserJWT string
}

// ToolOutcome is what the dispatcher returns. Parts allows the tool to
// return multi-format output (text + image for a screenshot tool, etc.).
// Status is "completed" or "errored" — never "pending" (that's
// transient between EventToolCall and EventToolResult).
type ToolOutcome struct {
	Status     string                     // "completed" | "errored"
	Parts      []sessionstore.MessagePart // multi-format result (LLM-visible)
	Error      string                     // non-empty when Status="errored"
	DurationMs int64                      // wall-clock from Dispatch to return
	// Diff and Metadata are CLIENT-ONLY : forwarded to the UI on the tool_result
	// event, never folded into Parts, so the LLM never sees them.
	Diff     *tool.DiffView // file mutation diff (edit/write) ; nil otherwise
	Metadata map[string]any // structured side-channel (bytes_written, …)
}

// NoopDispatcher is the default when no real dispatcher is wired. Every
// call returns an "errored" outcome explaining the tool isn't bound.
// Lets the runtime path stay alive in dev / smoke tests without a
// module registry, while making the missing wiring loud (the LLM sees
// the error and can decide to give up).
type NoopDispatcher struct{}

func (NoopDispatcher) Dispatch(_ context.Context, call ToolInvocation) ToolOutcome {
	return ToolOutcome{
		Status: "errored",
		Error:  "tool dispatcher not wired (tool=" + call.Name + ")",
	}
}

// StaticToolDispatcher is a test helper : map tool name → fixed
// outcome. Lets unit tests assert what the runtime does with success /
// error / multi-format outcomes without spinning up real modules.
type StaticToolDispatcher struct {
	Outcomes map[string]ToolOutcome
}

func (s *StaticToolDispatcher) Dispatch(_ context.Context, call ToolInvocation) ToolOutcome {
	if s.Outcomes == nil {
		return ToolOutcome{Status: "errored", Error: "no outcome configured for " + call.Name}
	}
	out, ok := s.Outcomes[call.Name]
	if !ok {
		return ToolOutcome{Status: "errored", Error: "no outcome configured for " + call.Name}
	}
	return out
}
