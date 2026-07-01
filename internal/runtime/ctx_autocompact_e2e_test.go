package runtime_test

import (
	"context"
	"testing"

	"sync"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/contextcompact"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// truncatingCompactor runs the SAME truncate path as the production
// SessionCompactor (contextcompact.Truncate + a durable EventContextCompacted),
// so a test proves a REAL compaction — not a recorded stub call.
type truncatingCompactor struct {
	sess   *projectingSessions
	mu     sync.Mutex
	called int
}

func (c *truncatingCompactor) CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error {
	c.mu.Lock()
	c.called++
	c.mu.Unlock()
	st, err := c.sess.State(sessionID)
	if err != nil || st == nil {
		return err
	}
	snap := st.Snapshot()
	res := contextcompact.Truncate(snap.Messages, contextcompact.KeepRecentOrDefault(keepLast), snap.Goal)
	if res.Dropped == 0 {
		return nil
	}
	_, err = c.sess.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventContextCompacted,
		SessionID: sessionID,
		CtxCompact: &sessionstore.ContextCompactPayload{
			CutoffSeq: res.CutoffSeq, Summary: res.Summary, KeepRecent: keepLast,
			Strategy: res.Strategy, MessagesDropped: res.Dropped,
		},
	})
	return err
}

func (c *truncatingCompactor) timesCalled() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.called
}

// autoCompactHook builds the synthetic hook exactly as the compiler injects it
// for runtime.context.auto_compact (context_pressure → compact_context).
func autoCompactHook(threshold float64) schema.Hook {
	return schema.Hook{
		ID: "_auto_compact",
		On: schema.HookEventTurnStart,
		Condition: schema.HookCondition{
			Type:   schema.CondContextPressure,
			Params: map[string]any{"threshold": threshold},
		},
		Action: schema.HookAction{
			Type:   schema.ActionCompactContext,
			Params: map[string]any{"strategy": "truncate", "keep_last": 2},
		},
	}
}

// TestCTX_AutoCompactFiresUnderRealPressure is the CORE end-to-end proof: a real
// turn where the session's actual token usage pushes pressure past the YAML
// threshold must fire the injected _auto_compact hook and trigger a REAL
// compaction — not a stub. This is the feature, exercised through the whole
// engine: turn_start → computeHookMetrics → context_pressure → compact_context.
func TestCTX_AutoCompactFiresUnderRealPressure(t *testing.T) {
	app := realDispatchApp()
	// Small window so a modest seeded usage crosses the threshold deterministically.
	app.Definition.Agents[0].Brain.Context = &schema.ContextConfig{MaxTokens: 1000}
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-ac")
	ctx := context.Background()

	appendUserMsg(t, sess, "sess-ac", "MSG-1")
	appendUserMsg(t, sess, "sess-ac", "MSG-2")
	appendUserMsg(t, sess, "sess-ac", "MSG-3")
	// REAL provider token usage → pressure = 900/1000 = 0.9 > 0.5 threshold.
	if _, err := sess.AppendDurable(ctx, sessionstore.Event{
		Type: sessionstore.EventTokenUsage, SessionID: "sess-ac",
		Cost: &sessionstore.CostPayload{TokensIn: 700, TokensOut: 200},
	}); err != nil {
		t.Fatal(err)
	}

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done"}}
	e := newEngine(t, apps, sess, lc)
	comp := &truncatingCompactor{sess: sess} // runs the REAL truncate code
	e.Hooks = &hookSourceWith{eng: hooks.New(
		[]schema.Hook{autoCompactHook(0.5)},
		hooks.ActionDeps{Logger: silentRT4Logger{}, Compactor: comp},
	)}

	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-ac", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.timesCalled() < 1 {
		t.Fatal("auto_compact did NOT fire under real pressure (0.9 > 0.5) — the core feature is broken")
	}

	// PROOF that a REAL compaction happened : a durable EventContextCompacted
	// marker now exists, and the LLM-visible view drops the oldest message.
	full := mustState(t, sess, "sess-ac")
	if full.ContextCompaction == nil || full.ContextCompaction.CutoffSeq == 0 {
		t.Fatalf("no real compaction marker after the turn: %+v", full.ContextCompaction)
	}
	view := contextcompact.ApplyView(full.Messages, full.ContextCompaction.CutoffSeq, full.ContextCompaction.Summary)
	var sawMsg1 bool
	for _, m := range view {
		if m.Content == "MSG-1" {
			sawMsg1 = true
		}
	}
	if sawMsg1 {
		t.Error("oldest message MSG-1 still in the compacted view — compaction did not drop it")
	}
	// Durable history MUST keep everything (audit). The in-memory window is now
	// bounded to the model's view (CTXLOAD : the context loads from the last
	// compaction, not all of history), so the full transcript lives in the
	// durable event log — verify MSG-1 survives there, reconstructed exactly as
	// /history rebuilds it.
	sess.mu.Lock()
	durable := sessionstore.TranscriptFromParts(nil, append([]sessionstore.Event(nil), sess.events...))
	sess.mu.Unlock()
	var diskHasMsg1 bool
	for _, m := range durable {
		if m.Content == "MSG-1" {
			diskHasMsg1 = true
		}
	}
	if !diskHasMsg1 {
		t.Error("compaction destroyed durable history — MSG-1 missing from the durable transcript")
	}
	t.Logf("PROVEN: pressure 0.9>0.5 → _auto_compact fired → real compaction (cutoffSeq=%d, dropped from view, kept on disk)", full.ContextCompaction.CutoffSeq)
}

func mustState(t *testing.T, sess *projectingSessions, sid string) sessionstore.SessionSnapshot {
	t.Helper()
	st, err := sess.State(sid)
	if err != nil || st == nil {
		t.Fatalf("State(%s): %v", sid, err)
	}
	return st.Snapshot()
}

// TestCTX_NoAutoCompactBelowThreshold is the negative: below the threshold the
// hook must NOT fire, so no compaction happens.
func TestCTX_NoAutoCompactBelowThreshold(t *testing.T) {
	app := realDispatchApp()
	app.Definition.Agents[0].Brain.Context = &schema.ContextConfig{MaxTokens: 1000}
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-ac2")
	ctx := context.Background()

	appendUserMsg(t, sess, "sess-ac2", "MSG-1")
	// Low usage → pressure = 100/1000 = 0.1 < 0.5.
	if _, err := sess.AppendDurable(ctx, sessionstore.Event{
		Type: sessionstore.EventTokenUsage, SessionID: "sess-ac2",
		Cost: &sessionstore.CostPayload{TokensIn: 100},
	}); err != nil {
		t.Fatal(err)
	}

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done"}}
	e := newEngine(t, apps, sess, lc)
	comp := &recordingCompactor{sess: sess}
	e.Hooks = &hookSourceWith{eng: hooks.New(
		[]schema.Hook{autoCompactHook(0.5)},
		hooks.ActionDeps{Logger: silentRT4Logger{}, Compactor: comp},
	)}

	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-ac2", UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.timesCalled() != 0 {
		t.Fatalf("compaction fired below threshold (0.1 < 0.5): called=%d", comp.timesCalled())
	}
}
