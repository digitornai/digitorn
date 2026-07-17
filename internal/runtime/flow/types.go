package flow

import "github.com/digitornai/digitorn/internal/runtime/sessionstore"

type FlowResult struct {
	Content string
	Seq     uint64
}

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

type AgentResult struct {
	Status  string
	Content string
	Error   string
}

type ToolInvocation struct {
	CallID    string
	Name      string
	Args      map[string]any
	AppID     string
	UserID    string
	SessionID string
	UserJWT   string
}

type ToolOutcome struct {
	Status string
	Parts  []sessionstore.MessagePart
	Error  string
}
