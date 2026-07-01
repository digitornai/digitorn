package tui

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

// TestShimmer_LiveTokenCounter proves the CLI chain : an assistant_delta
// carrying live_output_tokens drives the working indicator's token counter
// (with a ~ marking the estimate), and the token_usage anchor snaps it to the
// exact count (dropping the ~). This is what shows above the input.
func TestShimmer_LiveTokenCounter(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "s1"}
	s.messages.SetSize(80, 20)

	s.handleEnvelope(client.Envelope{Type: "turn_started", SessionID: "s1", Seq: 1})
	if !s.shimmering() {
		t.Fatal("turn_started must put the screen in the working/shimmer state")
	}

	// A streaming delta with the daemon's running estimate.
	s.handleEnvelope(client.Envelope{
		Type: "assistant_delta", SessionID: "s1", Seq: 0, LiveOutputTokens: 42,
		Payload: map[string]any{"parts": []any{map[string]any{"type": "text", "text": "Hello there"}}},
	})
	got := stripANSI(s.renderShimmer())
	if !strings.Contains(got, "~42 tokens") {
		t.Fatalf("live estimate not shown on the indicator: %q", got)
	}

	// The exact provider usage lands → snap, drop the ~.
	s.handleEnvelope(client.Envelope{
		Type: "token_usage", SessionID: "s1", Seq: 2,
		Payload: map[string]any{"tokens_in": float64(900), "tokens_out": float64(50)},
	})
	got = stripANSI(s.renderShimmer())
	if !strings.Contains(got, "50 tokens") || strings.Contains(got, "~50") {
		t.Fatalf("exact count not snapped on the indicator (want '50 tokens' without ~): %q", got)
	}

	// A new turn resets the counter.
	s.handleEnvelope(client.Envelope{Type: "turn_started", SessionID: "s1", Seq: 3})
	if s.turnTokens != 0 || s.turnTokensExact {
		t.Fatalf("turn_started must reset the counter: tokens=%d exact=%v", s.turnTokens, s.turnTokensExact)
	}
}
