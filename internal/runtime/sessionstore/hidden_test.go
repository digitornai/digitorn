package sessionstore

import "testing"

func TestHiddenFromClient(t *testing.T) {
	inject := Event{
		Type: EventSystemMessage,
		Message: &MessagePayload{
			Role:    "system",
			Content: "The user sent a new message while you were working…",
			Extra:   map[string]any{"source": "mid_turn_inject", "position": "append"},
		},
	}
	if !inject.HiddenFromClient() {
		t.Fatal("mid_turn_inject directive must be hidden from the client")
	}

	// A regular user message is never hidden.
	user := Event{Type: EventUserMessage, Message: &MessagePayload{Role: "user", Content: "hi"}}
	if user.HiddenFromClient() {
		t.Fatal("user messages must reach the client")
	}

	// A system message from another source (e.g. mode_switch) stays visible.
	other := Event{Type: EventSystemMessage, Message: &MessagePayload{
		Role: "system", Extra: map[string]any{"source": "mode_switch"},
	}}
	if other.HiddenFromClient() {
		t.Fatal("only the listed internal sources are hidden")
	}

	// A system message with no Extra is not hidden.
	bare := Event{Type: EventSystemMessage, Message: &MessagePayload{Role: "system"}}
	if bare.HiddenFromClient() {
		t.Fatal("a system message without a hidden source must not be filtered")
	}
}
