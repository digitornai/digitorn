package runtime

import "context"

// Recorder receives real-time, per-call telemetry from a running turn so a live
// registry (the AgentManager) can surface tool-call counts and token usage
// WITHOUT slowing the hot path : implementations do atomic adds only, never
// I/O or locks held across work. The AgentManager injects one into a
// sub-agent's turn context when it spawns it ; plain turns have none (nil).
type Recorder interface {
	AddLLMCall(promptTokens, completionTokens int)
	// AddToolCall increments the tool-call counter. toolName is the canonical
	// FQN of the tool being called (e.g. "filesystem.read") so live telemetry
	// can surface the agent's current activity; pass "" when unknown.
	AddToolCall(toolName string)
}

type recorderKey struct{}

// WithRecorder attaches a telemetry recorder to ctx.
func WithRecorder(ctx context.Context, r Recorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFromContext returns the attached recorder, or nil.
func RecorderFromContext(ctx context.Context) Recorder {
	r, _ := ctx.Value(recorderKey{}).(Recorder)
	return r
}
