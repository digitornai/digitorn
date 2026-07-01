package turn

import (
	"context"
	"fmt"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// RecoveryReason is the string written to EventTurnEnded.Reason and
// EventError.Code when a stale in-flight turn is closed at recovery
// time. Kept as an exported constant so observability dashboards can
// filter on it.
const RecoveryReason = "daemon_restarted"

// RecoverInFlight inspects the session snapshot and, if a turn was
// in-flight at the time of the snapshot (CurrentTurnPhase ∈
// {loading, running, persisting}), closes it cleanly by emitting :
//
//  1. EventError{code: "daemon_restarted", source: "turn.recovery"}
//     so the audit log + frontend show why this turn died.
//  2. EventTurnEnded{status: "errored", reason: "daemon_restarted",
//     turn_id: <stale ID>} so the projection clears CurrentTurn*.
//
// Idempotent : no-op when there is no in-flight turn (the common
// case). Designed to be called lazily by the orchestrator the first
// time a session is touched after daemon restart — avoids a boot-time
// scan of every session on disk (would be O(N) on the cold path,
// untenable at 1M sessions).
//
// Returns the number of stale turns recovered (0 or 1 — a session
// has at most one in-flight turn at any time by design).
func RecoverInFlight(ctx context.Context, snap sessionstore.SessionSnapshot, sink EventSink) (int, error) {
	if snap.CurrentTurnID == "" {
		return 0, nil
	}
	phase := Phase(snap.CurrentTurnPhase)
	if !phase.IsInFlight() {
		return 0, nil
	}
	stale := snap.CurrentTurnID

	// 1. Audit-grade error event.
	errEv := sessionstore.Event{
		Type:          sessionstore.EventError,
		SessionID:     snap.SessionID,
		AppID:         snap.AppID,
		UserID:        snap.UserID,
		CorrelationID: stale,
		Error: &sessionstore.ErrorPayload{
			Code:    RecoveryReason,
			Source:  "turn.recovery",
			Message: fmt.Sprintf("turn %s was in phase %q at daemon restart", stale, phase),
		},
	}
	if _, err := sink.AppendDurable(ctx, errEv); err != nil {
		return 0, fmt.Errorf("turn.recovery: emit error: %w", err)
	}

	// 2. Terminal event that the projection acts on to clear the
	// CurrentTurn* fields. Must match the stale TurnID so the
	// projection guard accepts it.
	endEv := sessionstore.Event{
		Type:          sessionstore.EventTurnEnded,
		SessionID:     snap.SessionID,
		AppID:         snap.AppID,
		UserID:        snap.UserID,
		CorrelationID: stale,
		Turn: &sessionstore.TurnPayload{
			TurnID: stale,
			Status: "errored",
			Reason: RecoveryReason,
		},
	}
	if _, err := sink.AppendDurable(ctx, endEv); err != nil {
		return 0, fmt.Errorf("turn.recovery: emit ended: %w", err)
	}
	return 1, nil
}
