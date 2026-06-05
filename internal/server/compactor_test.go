package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// newCompactorTestBus spins a real session bus on a temp dir.
func newCompactorTestBus(t *testing.T) *sessionstore.Bus {
	t.Helper()
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 4096,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond, Fsync: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := sessionstore.NewBus(sessionstore.BusConfig{
		Paths: paths, Flusher: flusher,
		EvictionInterval: time.Hour, StateIdleEvictAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})
	return bus
}

func seedMessages(t *testing.T, bus *sessionstore.Bus, sid string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 1; i <= n; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		_, err := bus.AppendDurable(ctx, sessionstore.Event{
			Type:      sessionstore.EventUserMessage, // projected into Messages regardless
			SessionID: sid,
			Message: &sessionstore.MessagePayload{
				Role: role, Content: fmt.Sprintf("message number %d body text", i),
			},
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// TestCompactor_TruncateEmitsDurableMarker : the production compactor
// reads the session, runs truncate, and emits a context_compacted event
// the projection records — making compact_context REAL.
func TestCompactor_TruncateEmitsDurableMarker(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "sess-compact-1"
	seedMessages(t, bus, sid, 8)

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.CompactSession(context.Background(), sid, "truncate", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	st, err := bus.State(sid)
	if err != nil {
		t.Fatal(err)
	}
	snap := st.Snapshot()
	if snap.ContextCompaction == nil {
		t.Fatal("no ContextCompaction marker after compaction")
	}
	// 8 messages, keepRecent 2 → drop first 6 → cutoff = seq 6.
	if snap.ContextCompaction.CutoffSeq != 6 {
		t.Errorf("cutoff = %d, want 6", snap.ContextCompaction.CutoffSeq)
	}
	if snap.ContextCompaction.Strategy != "truncate" {
		t.Errorf("strategy = %q, want truncate", snap.ContextCompaction.Strategy)
	}
	// History preserved : compaction only records a view cutoff. The in-memory
	// window is now bounded to the model's view (CTXLOAD), so the full transcript
	// lives on disk — verify the earliest and latest seeded messages both survive
	// in the durable transcript, while the in-memory window is correctly trimmed.
	full, err := bus.Transcript(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 8 {
		t.Errorf("durable transcript shrank to %d messages — compaction must preserve the log", len(full))
	}
	if !messagesContain(full, "message number 1") || !messagesContain(full, "message number 8") {
		t.Error("compaction destroyed history — first/last seeded message missing from durable transcript")
	}
	if len(snap.Messages) != 2 {
		t.Errorf("in-memory window = %d messages, want 2 (kept after cutoff seq 6)", len(snap.Messages))
	}
}

func messagesContain(msgs []sessionstore.Message, needle string) bool {
	for i := range msgs {
		if msgs[i].Content == needle || (len(msgs[i].Content) >= len(needle) && containsStr(msgs[i].Content, needle)) {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestCompactor_SummarizeFallsBackWithoutLLM : strategy=summarize with no
// LLM client wired must NOT fail — it falls back to truncate so
// compaction always makes progress (total reliability).
func TestCompactor_SummarizeFallsBackWithoutLLM(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "sess-compact-2"
	seedMessages(t, bus, sid, 8)

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.CompactSession(context.Background(), sid, "summarize", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil {
		t.Fatal("no marker — summarize fallback did not compact")
	}
	if snap.ContextCompaction.Strategy != "truncate" {
		t.Errorf("strategy = %q, want truncate (fallback when no LLM)", snap.ContextCompaction.Strategy)
	}
}

// TestCompactor_NoOpOnShortSession : a session shorter than keep_recent
// must not emit a marker.
func TestCompactor_NoOpOnShortSession(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "sess-compact-3"
	seedMessages(t, bus, sid, 2)

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.CompactSession(context.Background(), sid, "truncate", 10); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction != nil {
		t.Errorf("short session must not compact, got marker %+v", snap.ContextCompaction)
	}
}

// TestCompactor_HookActionFiresCompaction is the FULL chain : a real
// compact_context hook fires through the real hooks engine, which calls
// the production compactor, which emits the durable marker. This proves
// compact_context is no longer a prod no-op.
func TestCompactor_HookActionFiresCompaction(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "sess-hook-compact"
	seedMessages(t, bus, sid, 8)

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng := hooks.New([]schema.Hook{{
		ID: "auto_compact", On: schema.HookEventTurnEnd,
		Condition: schema.HookCondition{Type: "always"},
		Action: schema.HookAction{Type: "compact_context", Params: map[string]any{
			"strategy": "truncate", "keep_last": 2,
		}},
	}}, hooks.ActionDeps{Compactor: c, Logger: discardActionLogger{}})
	eng.Async = false

	eng.Fire(context.Background(), schema.HookEventTurnEnd, nil, hooks.Payload{SessionID: sid})

	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil {
		t.Fatal("compact_context hook did not produce a compaction marker (still a no-op)")
	}
	if snap.ContextCompaction.CutoffSeq != 6 {
		t.Errorf("cutoff = %d, want 6", snap.ContextCompaction.CutoffSeq)
	}
}

type discardActionLogger struct{}

func (discardActionLogger) Info(string, ...any)  {}
func (discardActionLogger) Warn(string, ...any)  {}
func (discardActionLogger) Error(string, ...any) {}

func mustSnap(t *testing.T, bus *sessionstore.Bus, sid string) sessionstore.SessionSnapshot {
	t.Helper()
	st, err := bus.State(sid)
	if err != nil {
		t.Fatal(err)
	}
	return st.Snapshot()
}
