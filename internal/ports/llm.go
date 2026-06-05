// Package ports defines the interface contracts (hexagonal architecture).
// Adapters in internal/adapters/ implement these contracts; the core depends
// only on these interfaces.
package ports

import (
	"context"
)

// LLMMessage is a single message in a chat completion.
type LLMMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// LLMTool describes a tool available to the LLM.
type LLMTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"` // JSON Schema for parameters
}

// LLMToolCall is an LLM-issued request to invoke a tool.
type LLMToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded args
}

// LLMResponse is the result of a chat completion call.
type LLMResponse struct {
	Content      string        `json:"content"`
	ToolCalls    []LLMToolCall `json:"tool_calls,omitempty"`
	FinishReason string        `json:"finish_reason"`
	TokensIn     int           `json:"tokens_in"`
	TokensOut    int           `json:"tokens_out"`
}

// LLMRequest is a chat completion request.
type LLMRequest struct {
	System      string       `json:"system,omitempty"`
	Messages    []LLMMessage `json:"messages"`
	Tools       []LLMTool    `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Model       string       `json:"model"`
}

// LLMStreamChunk is one streamed token (or tool call delta).
type LLMStreamChunk struct {
	Delta        string        `json:"delta,omitempty"`
	ToolCalls    []LLMToolCall `json:"tool_calls,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

// LLMProvider abstracts an LLM backend (OpenAI, Anthropic, Ollama, ...).
type LLMProvider interface {
	// Name returns the provider identifier ("openai", "anthropic", ...).
	Name() string

	// Complete performs a single (non-streaming) chat completion.
	Complete(ctx context.Context, req LLMRequest) (LLMResponse, error)

	// Stream performs a streaming chat completion. The returned channel is
	// closed when the stream ends. If the context is canceled, the stream
	// is aborted.
	Stream(ctx context.Context, req LLMRequest) (<-chan LLMStreamChunk, error)
}
