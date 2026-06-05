package client

import "testing"

// TestDecodeEnvelope_LiveOutputTokens : the daemon emits live_output_tokens on
// assistant_delta (CTX-7.5). decodeEnvelope must carry it onto the typed
// Envelope (JSON numbers arrive as float64) so the TUI can render the live
// counter. If decode drops it, the counter stays frozen at 0.
func TestDecodeEnvelope_LiveOutputTokens(t *testing.T) {
	env := decodeEnvelope(map[string]any{
		"type":               "assistant_delta",
		"seq":                float64(0),
		"session_id":         "s1",
		"live_output_tokens": float64(42),
		"payload": map[string]any{
			"parts": []any{map[string]any{"type": "text", "text": "hi"}},
		},
	})
	if env.LiveOutputTokens != 42 {
		t.Fatalf("live_output_tokens dropped by decode: got %d, want 42", env.LiveOutputTokens)
	}
}
