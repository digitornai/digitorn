package ports

import "context"

// MiddlewareContext is the data shared between middleware and the LLM call.
type MiddlewareContext struct {
	AgentID      string
	SessionID    string
	UserID       string
	Turn         int
	SystemPrompt string
	Messages     []LLMMessage
	Metadata     map[string]any
}

// AppMiddleware wraps an agent turn — Before runs prior to the LLM call (and
// may short-circuit it), After runs once the LLM response is in hand and may
// transform it.
type AppMiddleware interface {
	// Name identifies the middleware (matches its YAML config key).
	Name() string

	// Before is called prior to the LLM call. When shortCircuit is true the
	// chain stops, the LLM is NOT called, and the returned string becomes the
	// assistant response (the string may be empty — the bool, not emptiness,
	// decides, matching the reference daemon's `result is not None`).
	Before(ctx context.Context, mctx *MiddlewareContext) (response string, shortCircuit bool, err error)

	// After is called once the LLM has responded. The middleware may modify
	// the response or inspect tool calls; the returned string replaces the
	// response delivered to the agent loop.
	After(ctx context.Context, mctx *MiddlewareContext, response string, toolCalls []LLMToolCall) (string, error)
}
