package tui

import (
	"testing"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

// A turn in progress must keep the animation loop alive even when nothing
// else is animating — no toasts, tokens already streaming (shimmer off), no
// running chip. Without this the tick stops during a quiet mid-turn gap and
// the working indicator freezes while the turn is still running.
func TestAnimating_PendingTurnKeepsTickAlive(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "s1"}

	if s.animating() {
		t.Fatal("idle screen should not animate")
	}

	// Turn in flight, a preamble already streamed (streamBuf != ""), no running
	// chip : the working indicator must STAY visible/animated (it used to vanish
	// here, freezing while the model generated the tool call).
	s.pendingTurn = true
	s.streamBuf = "partial reply so far"
	if !s.shimmering() {
		t.Fatal("working indicator must stay on during a pending turn (no chip running)")
	}
	if !s.animating() {
		t.Fatal("a pending turn must keep animating() true so the tick never freezes mid-turn")
	}

	// A tool chip is now executing (its tool_result hasn't landed). The chip's
	// own "running…" suffix is STATIC, so the sweeping working indicator must
	// keep going — the whole tool phase used to look frozen otherwise.
	s.messages.SetMessages([]client.Message{
		{Role: "tool", Content: "filesystem.read", CallID: "c1", Status: "running", Seq: 1},
	})
	if !s.messages.HasRunning() {
		t.Fatal("a running chip should report HasRunning() true")
	}
	if !s.shimmering() {
		t.Fatal("working indicator must stay on WHILE a tool chip executes (static chip text doesn't animate)")
	}

	// Turn ends → back to idle, tick is free to stop.
	s.pendingTurn = false
	s.streamBuf = ""
	s.messages.SetMessages(nil)
	if s.animating() {
		t.Fatal("after the turn ends with nothing else animating, animating() must be false")
	}
}
