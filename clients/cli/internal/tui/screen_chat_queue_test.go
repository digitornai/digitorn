package tui

import (
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// Messages typed while a turn is in flight queue locally (FIFO) and drain one
// per turn, instead of all firing at the daemon (which would coalesce them).
func TestSendQueue_OnePerTurn(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "s1"}

	// Idle : the first message sends now and optimistically marks the turn busy.
	if cmd := s.sendOrQueue("m1"); cmd == nil {
		t.Fatal("first send should return a post command")
	}
	if !s.pendingTurn {
		t.Fatal("first send must set pendingTurn optimistically")
	}
	if len(s.sendQueue) != 0 {
		t.Fatalf("first send must not queue, got %d", len(s.sendQueue))
	}

	// Busy : the next two queue instead of sending.
	s.sendOrQueue("m2")
	s.sendOrQueue("m3")
	if len(s.sendQueue) != 2 || s.sendQueue[0] != "m2" || s.sendQueue[1] != "m3" {
		t.Fatalf("queue=%v want [m2 m3]", s.sendQueue)
	}

	// Dequeue while still busy is a no-op.
	if s.dequeueSend() != nil {
		t.Fatal("dequeue while pendingTurn must be a no-op")
	}

	// Turn ends → next message (FIFO) sends and re-arms the busy flag.
	s.pendingTurn = false
	if cmd := s.dequeueSend(); cmd == nil {
		t.Fatal("dequeue when idle should send the next queued message")
	}
	if !s.pendingTurn {
		t.Fatal("dequeue should re-set pendingTurn for the message it just sent")
	}
	if len(s.sendQueue) != 1 || s.sendQueue[0] != "m3" {
		t.Fatalf("queue=%v want [m3] after draining one", s.sendQueue)
	}

	// Next turn ends → last message drains, queue empties.
	s.pendingTurn = false
	s.dequeueSend()
	if len(s.sendQueue) != 0 {
		t.Fatalf("queue should be empty, got %v", s.sendQueue)
	}

	// Empty queue → no-op even when idle.
	s.pendingTurn = false
	if s.dequeueSend() != nil {
		t.Fatal("dequeue on empty queue must be a no-op")
	}
}
