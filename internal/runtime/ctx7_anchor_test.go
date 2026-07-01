package runtime_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// TestCTX7_AnchorDrivesCompactionViaRealEngine is the proof that the production
// gap is closed : BEFORE CTX-7 the engine only logged resp.Usage, so the
// occupancy gauge was 0 in prod and auto_compact NEVER fired on real pressure.
// Now the engine persists the provider's usage as the anchor → the gauge → the
// pressure. This test uses NO manual token_usage seeding : the engine itself
// emits it. Turn 1 sets the gauge (no compaction yet — turn_start saw 0) ;
// turn 2's turn_start sees the anchored gauge cross the threshold and fires a
// REAL compaction.
func TestCTX7_AnchorDrivesCompactionViaRealEngine(t *testing.T) {
	app := realDispatchApp()
	app.Definition.Agents[0].Brain.Context = &schema.ContextConfig{MaxTokens: 1000}
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-ctx7")
	ctx := context.Background()

	// The provider reports 950 tokens for the prompt+completion. 950/1000 = 0.95
	// > 0.5 threshold. This is the ONLY source of the number — no seeding.
	lc := &stubLLM{resp: &llm.ChatResponse{
		Content: "done",
		Usage:   llm.Usage{PromptTokens: 900, CompletionTokens: 50},
	}}
	comp := &truncatingCompactor{sess: sess}
	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: hooks.New(
		[]schema.Hook{autoCompactHook(0.5)},
		hooks.ActionDeps{Logger: silentRT4Logger{}, Compactor: comp},
	)}

	// --- Turn 1 : gauge starts at 0, so turn_start must NOT compact. ---
	appendUserMsg(t, sess, "sess-ctx7", "MSG-1")
	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-ctx7", UserID: "u"}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if comp.timesCalled() != 0 {
		t.Fatalf("turn 1 must not compact (gauge was 0 at its turn_start), called=%d", comp.timesCalled())
	}
	// PROOF the engine emitted the anchor itself : one token_usage event + the
	// occupancy gauge now reflects the provider's exact count (NOT cumulative).
	if got := sess.count(sessionstore.EventTokenUsage); got != 1 {
		t.Fatalf("engine must persist exactly one token_usage anchor per turn, got %d", got)
	}
	if g := mustState(t, sess, "sess-ctx7").ContextTokens; g != 950 {
		t.Fatalf("occupancy gauge = %d, want 950 (prompt 900 + completion 50, last-wins)", g)
	}

	// --- Turn 2 : the anchored gauge (950 > 500) must fire a real compaction. ---
	appendUserMsg(t, sess, "sess-ctx7", "MSG-2")
	appendUserMsg(t, sess, "sess-ctx7", "MSG-3")
	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-ctx7", UserID: "u"}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if comp.timesCalled() < 1 {
		t.Fatal("auto_compact did NOT fire on turn 2 despite the engine-anchored gauge crossing the threshold — the prod path is still broken")
	}
	full := mustState(t, sess, "sess-ctx7")
	if full.ContextCompaction == nil || full.ContextCompaction.CutoffSeq == 0 {
		t.Fatalf("no real compaction marker after turn 2: %+v", full.ContextCompaction)
	}
	t.Logf("PROVEN: engine emitted usage anchor (gauge=950) → turn-2 pressure 0.95>0.5 → real compaction (cutoffSeq=%d), zero manual seeding", full.ContextCompaction.CutoffSeq)
}

// TestCTX7_GaugeIsOccupancyNotCumulativeCost guards the core distinction : the
// occupancy gauge is last-value-wins (the size of the window now), NOT the sum
// of every turn's tokens (which is cost). Two usage events must leave the gauge
// at the LAST value, while the cost counters accumulate.
func TestCTX7_GaugeIsOccupancyNotCumulativeCost(t *testing.T) {
	s := sessionstore.NewSessionState("s1")

	sessionstore.Apply(s, &sessionstore.Event{
		Seq: 1, Type: sessionstore.EventTokenUsage, SessionID: "s1",
		Cost: &sessionstore.CostPayload{TokensIn: 300, TokensOut: 100},
	})
	sessionstore.Apply(s, &sessionstore.Event{
		Seq: 2, Type: sessionstore.EventTokenUsage, SessionID: "s1",
		Cost: &sessionstore.CostPayload{TokensIn: 500, TokensOut: 120},
	})

	// Occupancy = LAST round only (500+120), the real window size now.
	if s.ContextTokens != 620 {
		t.Errorf("occupancy gauge = %d, want 620 (last-wins, not summed)", s.ContextTokens)
	}
	// Cost = cumulative across both rounds (300+500 in, 100+120 out).
	if s.TokensIn != 800 || s.TokensOut != 220 {
		t.Errorf("cost counters = in:%d out:%d, want in:800 out:220 (cumulative)", s.TokensIn, s.TokensOut)
	}
}
