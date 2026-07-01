package flow

import "github.com/digitornai/digitorn/internal/runtime/sessionstore"

// FlowResult is what Run() returns to the engine.
type FlowResult struct {
	Content string
	Seq     uint64
}

// AgentSpec describes a sub-agent invocation without referencing the runtime package.
type AgentSpec struct {
	AppID         string
	ParentSession string
	UserID        string
	UserJWT       string
	AgentID       string
	RunID         string
	Task          string
	MemorySeed    string
}

// AgentResult is the outcome returned by RunAgent.
type AgentResult struct {
	Status  string
	Content string
	Error   string
}

// ToolInvocation describes a tool call without referencing the runtime package.
type ToolInvocation struct {
	CallID    string
	Name      string
	Args      map[string]any
	AppID     string
	UserID    string
	SessionID string
	UserJWT   string
}

// ToolOutcome is the result returned by RunTool.
type ToolOutcome struct {
	Status string
	Parts  []sessionstore.MessagePart
	Error  string
}
