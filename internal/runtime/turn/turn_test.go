package turn_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/turn"
)

// ---- fake sink ----

type fakeSink struct {
	mu     sync.Mutex
	events []sessionstore.Event
	err    error
	failOn sessionstore.EventType // if matches, return err once
	seq    atomic.Uint64
}

func (f *fakeSink) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && ev.Type == f.failOn {
		f.failOn = ""
		return 0, f.err
	}
	f.events = append(f.events, ev)
	return f.seq.Add(1), nil
}

func (f *fakeSink) snapshot() []sessionstore.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sessionstore.Event, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeSink) typesEmitted() []sessionstore.EventType {
	evs := f.snapshot()
	out := make([]sessionstore.EventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func newID() turn.IDGen {
	var n atomic.Int64
	return func() string {
		v := n.Add(1)
		return "turn-" + string(rune('0'+v))
	}
}

func newTurn(t *testing.T, sink *fakeSink) *turn.Turn {
	t.Helper()
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 16, PerAppCap: 4, PerUserCap: 2})
	tr, err := turn.New(turn.Options{
		SessionID: "sess-1",
		AppID:     "app-1",
		AgentID:   "main",
		UserID:    "user-A",
		UserJWT:   "tok-X",
		Pool:      p,
		Sink:      sink,
		IDGen:     newID(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

// ---- construction ----

func TestNew_RequiresSessionID(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	_, err := turn.New(turn.Options{
		Pool: p, Sink: &fakeSink{}, IDGen: newID(),
	})
	if err == nil || !strings.Contains(err.Error(), "SessionID") {
		t.Fatalf("want SessionID error, got %v", err)
	}
}

func TestNew_RequiresPool(t *testing.T) {
	_, err := turn.New(turn.Options{
		SessionID: "s", Sink: &fakeSink{}, IDGen: newID(),
	})
	if err == nil || !strings.Contains(err.Error(), "Pool") {
		t.Fatalf("want Pool error, got %v", err)
	}
}

func TestNew_RequiresSink(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	_, err := turn.New(turn.Options{
		SessionID: "s", Pool: p, IDGen: newID(),
	})
	if err == nil || !strings.Contains(err.Error(), "Sink") {
		t.Fatalf("want Sink error, got %v", err)
	}
}

func TestNew_RequiresIDGen(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	_, err := turn.New(turn.Options{
		SessionID: "s", Pool: p, Sink: &fakeSink{},
	})
	if err == nil || !strings.Contains(err.Error(), "IDGen") {
		t.Fatalf("want IDGen error, got %v", err)
	}
}

// ---- Start ----

func TestStart_EmitsTurnStarted(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].Type != sessionstore.EventTurnStarted {
		t.Errorf("event type = %q", evs[0].Type)
	}
	if evs[0].Turn == nil || evs[0].Turn.TurnID == "" {
		t.Errorf("turn payload missing : %+v", evs[0])
	}
	if evs[0].CorrelationID != evs[0].Turn.TurnID {
		t.Errorf("correlation_id must equal TurnID")
	}
}

func TestStart_TwiceErrors(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Start(context.Background()); err == nil {
		t.Error("expected error on double Start")
	}
}

func TestStart_PoolFullReturnsError(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	sink := &fakeSink{}
	tr1, _ := turn.New(turn.Options{
		SessionID: "s1", Pool: p, Sink: sink, IDGen: newID(),
	})
	if err := tr1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr2, _ := turn.New(turn.Options{
		SessionID: "s2", Pool: p, Sink: sink, IDGen: newID(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr2.Start(ctx); err == nil {
		t.Fatal("expected pool full error")
	}
}

func TestStart_SinkErrorReleasesPool(t *testing.T) {
	sink := &fakeSink{failOn: sessionstore.EventTurnStarted, err: errors.New("disk full")}
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	tr, _ := turn.New(turn.Options{
		SessionID: "s", AppID: "a", UserID: "u",
		Pool: p, Sink: sink, IDGen: newID(),
	})
	if err := tr.Start(context.Background()); err == nil {
		t.Fatal("expected sink error")
	}
	// Pool should be free now — another Start should not block.
	tr2, _ := turn.New(turn.Options{
		SessionID: "s2", AppID: "a", UserID: "u",
		Pool: p, Sink: &fakeSink{}, IDGen: newID(),
	})
	if err := tr2.Start(context.Background()); err != nil {
		t.Fatalf("pool not released after start failure: %v", err)
	}
}

// ---- TransitionTo ----

func TestTransitionTo_HappyChain(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []turn.Phase{turn.PhaseLoading, turn.PhaseRunning, turn.PhasePersisting} {
		if err := tr.TransitionTo(context.Background(), p); err != nil {
			t.Fatalf("TransitionTo %q: %v", p, err)
		}
		if tr.Phase() != p {
			t.Errorf("Phase() = %q, want %q", tr.Phase(), p)
		}
	}
	// Verify event sequence : 1 Started + 3 PhaseChanged.
	types := sink.typesEmitted()
	want := []sessionstore.EventType{
		sessionstore.EventTurnStarted,
		sessionstore.EventTurnPhaseChanged,
		sessionstore.EventTurnPhaseChanged,
		sessionstore.EventTurnPhaseChanged,
	}
	if len(types) != len(want) {
		t.Fatalf("len = %d, want %d ; got %v", len(types), len(want), types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("types[%d] = %q, want %q", i, types[i], want[i])
		}
	}
}

func TestTransitionTo_IllegalSkipRejected(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.TransitionTo(context.Background(), turn.PhasePersisting); err == nil {
		t.Error("skipping Loading→Running→ should be rejected")
	}
	// Phase must not have changed.
	if tr.Phase() != turn.PhasePending {
		t.Errorf("Phase advanced on illegal transition : %q", tr.Phase())
	}
}

func TestTransitionTo_SameRejected(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.TransitionTo(context.Background(), turn.PhaseLoading); err != nil {
		t.Fatal(err)
	}
	if err := tr.TransitionTo(context.Background(), turn.PhaseLoading); err == nil {
		t.Error("same-phase transition must be rejected")
	}
}

func TestTransitionTo_BeforeStartErrors(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	if err := tr.TransitionTo(context.Background(), turn.PhaseLoading); err == nil {
		t.Error("TransitionTo before Start must error")
	}
}

// ---- CloseDone / Fail / Interrupt ----

func TestCloseDone_EmitsEndedAndReleasesPool(t *testing.T) {
	sink := &fakeSink{}
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	tr, _ := turn.New(turn.Options{
		SessionID: "s", AppID: "a", UserID: "u",
		Pool: p, Sink: sink, IDGen: newID(),
	})
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, ph := range []turn.Phase{turn.PhaseLoading, turn.PhaseRunning, turn.PhasePersisting} {
		_ = tr.TransitionTo(context.Background(), ph)
	}
	if err := tr.CloseDone(context.Background()); err != nil {
		t.Fatalf("CloseDone: %v", err)
	}
	// Last event must be Ended with status=done.
	evs := sink.snapshot()
	last := evs[len(evs)-1]
	if last.Type != sessionstore.EventTurnEnded {
		t.Fatalf("last event = %q", last.Type)
	}
	if last.Turn.Status != "done" {
		t.Errorf("status = %q", last.Turn.Status)
	}
	// Pool slot released — another acquire should succeed.
	tr2, _ := turn.New(turn.Options{
		SessionID: "s2", Pool: p, Sink: &fakeSink{}, IDGen: newID(),
	})
	if err := tr2.Start(context.Background()); err != nil {
		t.Fatalf("pool not released: %v", err)
	}
}

func TestCloseDone_IsIdempotent(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	_ = tr.Start(context.Background())
	// Walk the canonical path : pending → loading → running →
	// persisting. CloseDone then transitions persisting → done.
	for _, p := range []turn.Phase{turn.PhaseLoading, turn.PhaseRunning, turn.PhasePersisting} {
		if err := tr.TransitionTo(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.CloseDone(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second call : no-op, no extra event.
	before := len(sink.snapshot())
	if err := tr.CloseDone(context.Background()); err != nil {
		t.Errorf("second CloseDone errored: %v", err)
	}
	after := len(sink.snapshot())
	if before != after {
		t.Errorf("idempotent CloseDone emitted extra event(s) : %d -> %d", before, after)
	}
}

func TestFail_EmitsErroredWithReason(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	_ = tr.Start(context.Background())
	cause := errors.New("upstream 500")
	if err := tr.Fail(context.Background(), cause); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	last := sink.snapshot()[len(sink.snapshot())-1]
	if last.Type != sessionstore.EventTurnEnded {
		t.Fatalf("last = %q", last.Type)
	}
	if last.Turn.Status != "errored" {
		t.Errorf("status = %q", last.Turn.Status)
	}
	if last.Turn.Reason != "upstream 500" {
		t.Errorf("reason = %q", last.Turn.Reason)
	}
}

func TestFail_FromAnyNonTerminalPhase(t *testing.T) {
	chain := []turn.Phase{
		turn.PhasePending, // starting point — Start sets this
		turn.PhaseLoading,
		turn.PhaseRunning,
		turn.PhasePersisting,
	}
	for idx, startPhase := range chain {
		t.Run(string(startPhase), func(t *testing.T) {
			sink := &fakeSink{}
			tr := newTurn(t, sink)
			if err := tr.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Walk to startPhase (skip the first since Start leaves us
			// at pending already).
			for i := 1; i <= idx; i++ {
				if err := tr.TransitionTo(context.Background(), chain[i]); err != nil {
					t.Fatalf("walk to %q: %v", chain[i], err)
				}
			}
			if tr.Phase() != startPhase {
				t.Fatalf("setup failed : phase = %q, want %q", tr.Phase(), startPhase)
			}
			if err := tr.Fail(context.Background(), errors.New("boom")); err != nil {
				t.Errorf("Fail from %q failed : %v", startPhase, err)
			}
		})
	}
}

func TestInterrupt_EmitsInterruptedWithReason(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	_ = tr.Start(context.Background())
	if err := tr.Interrupt(context.Background(), "user clicked stop"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	last := sink.snapshot()[len(sink.snapshot())-1]
	if last.Turn.Status != "interrupted" || last.Turn.Reason != "user clicked stop" {
		t.Errorf("payload = %+v", last.Turn)
	}
}

// ---- Pool release safety ----

func TestClose_ReleasesPoolEvenOnSinkError(t *testing.T) {
	sink := &fakeSink{failOn: sessionstore.EventTurnEnded, err: errors.New("disk full")}
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	tr, _ := turn.New(turn.Options{
		SessionID: "s", Pool: p, Sink: sink, IDGen: newID(),
	})
	_ = tr.Start(context.Background())
	// Close emits two events (PhaseChanged for terminal + Ended) ;
	// the Ended one fails. Pool MUST still be released.
	_ = tr.CloseDone(context.Background())
	// Another Start should succeed.
	sink2 := &fakeSink{}
	tr2, _ := turn.New(turn.Options{
		SessionID: "s2", Pool: p, Sink: sink2, IDGen: newID(),
	})
	if err := tr2.Start(context.Background()); err != nil {
		t.Fatalf("pool leaked after sink error: %v", err)
	}
}

// ---- Correlation ID ----

func TestAllEvents_CarrySameTurnID(t *testing.T) {
	sink := &fakeSink{}
	tr := newTurn(t, sink)
	_ = tr.Start(context.Background())
	_ = tr.TransitionTo(context.Background(), turn.PhaseLoading)
	_ = tr.TransitionTo(context.Background(), turn.PhaseRunning)
	_ = tr.CloseDone(context.Background())

	turnID := tr.ID
	for i, ev := range sink.snapshot() {
		if ev.CorrelationID != turnID {
			t.Errorf("event[%d] correlation_id = %q, want %q", i, ev.CorrelationID, turnID)
		}
		if ev.Turn == nil || ev.Turn.TurnID != turnID {
			t.Errorf("event[%d] turn.TurnID mismatch : %+v", i, ev.Turn)
		}
	}
}
