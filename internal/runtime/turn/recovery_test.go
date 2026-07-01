package turn_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/turn"
)

func TestRecoverInFlight_NoTurnIsNoOp(t *testing.T) {
	sink := &fakeSink{}
	snap := sessionstore.SessionSnapshot{
		SessionID:     "s1",
		CurrentTurnID: "", // no turn in flight
	}
	n, err := turn.RecoverInFlight(context.Background(), snap, sink)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if len(sink.snapshot()) != 0 {
		t.Errorf("no event should be emitted ; got %d", len(sink.snapshot()))
	}
}

func TestRecoverInFlight_TerminalTurnIsNoOp(t *testing.T) {
	// CurrentTurnPhase = "done"/"errored"/"interrupted" shouldn't
	// normally happen (projection clears it) but be defensive.
	for _, p := range []string{"done", "errored", "interrupted"} {
		t.Run(p, func(t *testing.T) {
			sink := &fakeSink{}
			snap := sessionstore.SessionSnapshot{
				SessionID:        "s1",
				CurrentTurnID:    "stale",
				CurrentTurnPhase: p,
			}
			n, err := turn.RecoverInFlight(context.Background(), snap, sink)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 || len(sink.snapshot()) != 0 {
				t.Errorf("terminal phase should be no-op : n=%d evs=%d", n, len(sink.snapshot()))
			}
		})
	}
}

func TestRecoverInFlight_InFlightEmitsErrorAndEnded(t *testing.T) {
	for _, p := range []string{"loading", "running", "persisting"} {
		t.Run(p, func(t *testing.T) {
			sink := &fakeSink{}
			snap := sessionstore.SessionSnapshot{
				SessionID:        "s1",
				AppID:            "app-1",
				UserID:           "user-A",
				CurrentTurnID:    "stale-" + p,
				CurrentTurnPhase: p,
			}
			n, err := turn.RecoverInFlight(context.Background(), snap, sink)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("n = %d, want 1", n)
			}
			evs := sink.snapshot()
			if len(evs) != 2 {
				t.Fatalf("expected 2 events (error + ended), got %d", len(evs))
			}
			// Order matters : error first, then ended.
			if evs[0].Type != sessionstore.EventError {
				t.Errorf("evs[0] = %q, want error", evs[0].Type)
			}
			if evs[0].Error == nil || evs[0].Error.Code != turn.RecoveryReason {
				t.Errorf("error payload : %+v", evs[0].Error)
			}
			if evs[1].Type != sessionstore.EventTurnEnded {
				t.Errorf("evs[1] = %q, want turn_ended", evs[1].Type)
			}
			if evs[1].Turn.TurnID != snap.CurrentTurnID {
				t.Errorf("ended turn_id = %q, want %q", evs[1].Turn.TurnID, snap.CurrentTurnID)
			}
			if evs[1].Turn.Status != "errored" || evs[1].Turn.Reason != turn.RecoveryReason {
				t.Errorf("ended payload : %+v", evs[1].Turn)
			}
			// Correlation ID on both events.
			for i, ev := range evs {
				if ev.CorrelationID != snap.CurrentTurnID {
					t.Errorf("evs[%d] correlation_id = %q, want %q", i, ev.CorrelationID, snap.CurrentTurnID)
				}
			}
		})
	}
}

// Integration : after recovery, project the events back into a
// SessionState and verify CurrentTurn* fields are cleared.
func TestRecoverInFlight_ProjectionClearsCurrentTurn(t *testing.T) {
	sink := &fakeSink{}
	snap := sessionstore.SessionSnapshot{
		SessionID:        "s1",
		CurrentTurnID:    "stale-1",
		CurrentTurnPhase: "running",
	}
	if _, err := turn.RecoverInFlight(context.Background(), snap, sink); err != nil {
		t.Fatal(err)
	}
	// Reconstruct state from the emitted events.
	state := sessionstore.NewSessionState(snap.SessionID)
	state.CurrentTurnID = snap.CurrentTurnID
	state.CurrentTurnPhase = snap.CurrentTurnPhase
	for _, ev := range sink.snapshot() {
		e := ev
		sessionstore.Apply(state, &e)
	}
	// After replaying : CurrentTurn* must be empty.
	if state.CurrentTurnID != "" {
		t.Errorf("CurrentTurnID not cleared : %q", state.CurrentTurnID)
	}
	if state.CurrentTurnPhase != "" {
		t.Errorf("CurrentTurnPhase not cleared : %q", state.CurrentTurnPhase)
	}
	// One ErrorEntry recorded.
	if len(state.Errors) != 1 {
		t.Errorf("Errors len = %d, want 1", len(state.Errors))
	}
}

func TestRecoverInFlight_IsIdempotent(t *testing.T) {
	// Calling twice on a snapshot that's been recovered (now no
	// CurrentTurnID) is a no-op.
	sink := &fakeSink{}
	snap := sessionstore.SessionSnapshot{
		SessionID:        "s1",
		CurrentTurnID:    "stale",
		CurrentTurnPhase: "running",
	}
	if _, err := turn.RecoverInFlight(context.Background(), snap, sink); err != nil {
		t.Fatal(err)
	}
	// Simulate the cleared state.
	snap2 := snap
	snap2.CurrentTurnID = ""
	snap2.CurrentTurnPhase = ""
	before := len(sink.snapshot())
	if _, err := turn.RecoverInFlight(context.Background(), snap2, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.snapshot()) != before {
		t.Errorf("second call emitted extra events ; want idempotent")
	}
}
