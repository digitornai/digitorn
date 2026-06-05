// Package middlewareplugin implements `custom` app-level middleware as an
// out-of-process gRPC plugin, so users add middleware without recompiling the
// daemon. It reuses the generic worker service (internal/module/service): a
// custom middleware is just a worker module exposing two tools, "before" and
// "after". The daemon side (Proxy) is a ports.AppMiddleware that forwards
// Before/After to the worker ; the worker side (Module) wraps a plugin author's
// Before/After funcs. Any language that speaks the gRPC ModuleService can host
// a plugin ; Go authors use Module() for a few-line implementation.
package middlewareplugin

import "github.com/mbathepaul/digitorn/internal/ports"

// Tool names the plugin module must expose.
const (
	ToolBefore = "before"
	ToolAfter  = "after"
)

// Message mirrors ports.LLMMessage on the wire.
type Message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolCall mirrors ports.LLMToolCall on the wire.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Context is the JSON form of ports.MiddlewareContext sent to the plugin.
type Context struct {
	AgentID      string         `json:"agent_id"`
	SessionID    string         `json:"session_id"`
	UserID       string         `json:"user_id"`
	Turn         int            `json:"turn"`
	SystemPrompt string         `json:"system_prompt"`
	Messages     []Message      `json:"messages"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// BeforeRequest is the "before" tool input.
type BeforeRequest struct {
	Context Context `json:"context"`
}

// BeforeResult is the "before" tool output. The plugin returns the (possibly
// mutated) prompt + messages — over a process boundary there is no in-place
// mutation, so the new state is carried back explicitly.
type BeforeResult struct {
	SystemPrompt string    `json:"system_prompt"`
	Messages     []Message `json:"messages"`
	Response     string    `json:"response,omitempty"`
	ShortCircuit bool      `json:"short_circuit,omitempty"`
}

// AfterRequest is the "after" tool input.
type AfterRequest struct {
	Context   Context    `json:"context"`
	Response  string     `json:"response"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// AfterResult is the "after" tool output.
type AfterResult struct {
	Response string `json:"response"`
}

func contextFromPorts(m *ports.MiddlewareContext) Context {
	return Context{
		AgentID: m.AgentID, SessionID: m.SessionID, UserID: m.UserID,
		Turn: m.Turn, SystemPrompt: m.SystemPrompt,
		Messages: messagesFromPorts(m.Messages), Metadata: m.Metadata,
	}
}

func messagesFromPorts(in []ports.LLMMessage) []Message {
	out := make([]Message, len(in))
	for i := range in {
		out[i] = Message{Role: in[i].Role, Content: in[i].Content, ToolCallID: in[i].ToolCallID, Name: in[i].Name}
	}
	return out
}

func messagesToPorts(in []Message) []ports.LLMMessage {
	out := make([]ports.LLMMessage, len(in))
	for i := range in {
		out[i] = ports.LLMMessage{Role: in[i].Role, Content: in[i].Content, ToolCallID: in[i].ToolCallID, Name: in[i].Name}
	}
	return out
}

func toolCallsFromPorts(in []ports.LLMToolCall) []ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolCall, len(in))
	for i := range in {
		out[i] = ToolCall{ID: in[i].ID, Name: in[i].Name, Arguments: in[i].Arguments}
	}
	return out
}
