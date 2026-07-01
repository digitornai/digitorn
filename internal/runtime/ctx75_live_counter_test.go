package runtime_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// TestCTX75_LiveOutputTokenCounterStreams proves CTX-7.5 : during streaming,
// each assistant delta carries a running token estimate that INCREASES at the
// rhythm chunks arrive, and the envelope (what the client receives) carries it.
// This is the smooth, incrementing token counter — a cheap chars/4 estimate on
// the hot path, snapped to the exact provider total at round end.
func TestCTX75_LiveOutputTokenCounterStreams(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-live")

	deltas := []string{"Hello, ", "world", "! Here are a few more tokens."}
	chunks := []*llm.ChatChunk{
		{Delta: deltas[0]},
		{Delta: deltas[1]},
		{Delta: deltas[2]},
		{FinishReason: "stop"},
	}
	ss := newStreamingStub(chunks...)
	e := newEngine(t, apps, sess, ss)
	e.Streaming = true

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-live", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Collect the live counts from the delta events, in order.
	var counts []int
	builder := sessionstore.NewEnvelopeBuilder("inst", nil)
	for i := range sess.events {
		ev := &sess.events[i]
		if ev.Type != sessionstore.EventAssistantDelta {
			continue
		}
		if ev.LiveOutputTokens <= 0 {
			t.Errorf("delta carries no live token count (=%d)", ev.LiveOutputTokens)
		}
		// The client receives the count via the envelope — prove it's carried.
		if env := builder.Build(ev); env.LiveOutputTokens != ev.LiveOutputTokens {
			t.Errorf("envelope dropped the live count: env=%d ev=%d", env.LiveOutputTokens, ev.LiveOutputTokens)
		}
		counts = append(counts, ev.LiveOutputTokens)
	}

	if len(counts) != 3 {
		t.Fatalf("expected 3 delta counts, got %d (%v)", len(counts), counts)
	}
	// Strictly increasing : the counter goes UP at the rhythm tokens arrive.
	for i := 1; i < len(counts); i++ {
		if counts[i] <= counts[i-1] {
			t.Errorf("live counter not strictly increasing: %v", counts)
		}
	}
	// The final running count equals the cumulative chars/4 estimate of all
	// deltas (the documented live heuristic).
	want := 0
	for _, d := range deltas {
		want += (len(d) + 3) / 4
	}
	if counts[len(counts)-1] != want {
		t.Errorf("final live count = %d, want cumulative estimate %d", counts[len(counts)-1], want)
	}
	t.Logf("PROVEN: live token counter streamed %v (chars/4), carried on the delta envelope, increasing per chunk", counts)
}
