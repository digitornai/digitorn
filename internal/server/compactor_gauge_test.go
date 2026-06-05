package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// TestCompactor_NoEstimateOnSyncPath proves the production rule : compaction
// (which runs synchronously on the turn_start hook path) does NOT set the
// occupancy gauge from an estimate. It only records the cutoff ; the EXACT new
// size lands later via the background Context Service (EventContextTokens). So
// after a compaction with no prior anchor, the gauge is untouched (0), never a
// guessed value.
func TestCompactor_NoEstimateOnSyncPath(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "sess-no-estimate"
	seedMessages(t, bus, sid, 8)

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.CompactSession(context.Background(), sid, "truncate", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil {
		t.Fatal("compaction must still record the cutoff marker")
	}
	if snap.ContextTokens != 0 {
		t.Fatalf("compaction must NOT set the occupancy gauge from an estimate, got %d", snap.ContextTokens)
	}
}
