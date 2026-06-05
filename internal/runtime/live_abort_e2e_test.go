//go:build live

package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// TestLiveAbort_StreamingTurnStopsAndSavesPartial : the abort primitive proven
// against the REAL gateway with REAL token streaming. A long generation is
// started, then aborted mid-stream by cancelling the turn context. We prove :
//
//   - the turn unwinds promptly (streaming is actually interrupted, not drained),
//   - no further deltas arrive once the abort settles,
//   - the partial answer generated before the abort is saved durably,
//   - the turn closes with status=interrupted (not errored).
func TestLiveAbort_StreamingTurnStopsAndSavesPartial(t *testing.T) {
	f := liveSetup(t)
	f.engine.Streaming = true // real token streaming through the worker

	// A long, line-by-line answer so there's a generation window to abort into.
	f.injectUser(t, "Write a long, detailed essay of at least 400 words about the "+
		"history and engineering of the Eiffel Tower. Put exactly one sentence per line.")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Run(ctx, dgruntime.TurnInput{
			AppID:     "live-app",
			SessionID: "live-sess",
			UserID:    "test-user",
			UserJWT:   f.userJWT,
		})
		done <- err
	}()

	// Wait until the model has streamed several deltas (generation under way).
	deadline := time.Now().Add(40 * time.Second)
	for f.session.countType(sessionstore.EventAssistantDelta) < 5 && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("turn finished before we could abort mid-stream (err=%v) — model too fast or not streaming", err)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	deltasAtAbort := f.session.countType(sessionstore.EventAssistantDelta)
	if deltasAtAbort < 5 {
		t.Skipf("model did not stream enough before timeout (%d deltas) — cannot test mid-stream abort", deltasAtAbort)
	}
	t.Logf("aborting after %d streamed deltas", deltasAtAbort)

	// ABORT mid-stream.
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("aborted turn must return an error")
		}
		t.Logf("turn unwound with: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("turn did not stop within 20s of abort — streaming was not interrupted")
	}

	// Streaming actually stopped : no new deltas after the abort settles.
	settled := f.session.countType(sessionstore.EventAssistantDelta)
	time.Sleep(750 * time.Millisecond)
	if after := f.session.countType(sessionstore.EventAssistantDelta); after != settled {
		t.Errorf("deltas kept arriving after abort (%d -> %d) — stream not cut", settled, after)
	}

	// The partial answer was saved durably (deltas are render-only ; the engine
	// consolidates the partial into a real assistant message on interrupt).
	partial := f.session.lastAssistantTextOf("live-sess")
	if strings.TrimSpace(partial) == "" {
		t.Fatal("partial streamed answer was not saved on abort")
	}
	t.Logf("partial answer saved (%d chars)", len(partial))

	// And the turn closed as interrupted, not errored.
	if !f.session.hasTurnStatus("interrupted") {
		t.Error("turn must close with status=interrupted after abort")
	}
}
