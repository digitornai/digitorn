package indexer

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// poisonSink fails to upsert one specific document id, succeeds for all others.
type poisonSink struct {
	poison string
	good   int64
}

func (p *poisonSink) Upsert(_ context.Context, _ string, docs []Document) error {
	for _, d := range docs {
		if d.ID == p.poison {
			return fmt.Errorf("poison doc %s", d.ID)
		}
	}
	atomic.AddInt64(&p.good, int64(len(docs)))
	return nil
}

func (p *poisonSink) Delete(context.Context, string, string) error { return nil }

// TestService_DeadLetter_IsolatesPoison proves a single bad document does not
// block the batch : good docs land, the poison doc is dead-lettered (counter +
// hook), and its cursor hash is not advanced so it retries next sync.
func TestService_DeadLetter_IsolatesPoison(t *testing.T) {
	registerLoad()
	svc := NewService(NewMemCursor(), 4)
	var dlDoc string
	svc.OnDeadLetter(func(_ SourceSpec, d Document, _ error) { dlDoc = d.ID })

	spec := SourceSpec{Name: "src", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 5}}
	sink := &poisonSink{poison: "src-2"} // fakeConn emits ids src-0..src-4

	rep, err := svc.Sync(context.Background(), spec, sink)
	if err != nil {
		t.Fatalf("sync errored on poison (should isolate, not fail): %v", err)
	}
	if g := atomic.LoadInt64(&sink.good); g != 4 {
		t.Fatalf("good docs upserted = %d, want 4 (5 minus the poison)", g)
	}
	if dlDoc != "src-2" {
		t.Fatalf("dead-letter hook doc = %q, want src-2", dlDoc)
	}
	if st := svc.Stats(); st.DeadLettered != 1 || st.DocsUpserted != 4 {
		t.Fatalf("stats: DeadLettered=%d DocsUpserted=%d, want 1 and 4", st.DeadLettered, st.DocsUpserted)
	}
	_ = rep

	// Second sync: nothing changed for the good docs (hash persisted), but the
	// poison still retries (its hash was never saved).
	atomic.StoreInt64(&sink.good, 0)
	if _, err := svc.Sync(context.Background(), spec, sink); err != nil {
		t.Fatal(err)
	}
	if g := atomic.LoadInt64(&sink.good); g != 0 {
		t.Fatalf("good docs re-upserted = %d, want 0 (unchanged → not re-sent)", g)
	}
	if st := svc.Stats(); st.DeadLettered != 2 {
		t.Fatalf("poison not retried: DeadLettered=%d, want 2", st.DeadLettered)
	}
}

// slowConn blocks in Walk until its context is cancelled or a short delay
// elapses — used to prove Shutdown drains in-flight work.
type slowConn struct{ done *int32 }

func (slowConn) Type() string      { return "slow" }
func (slowConn) Capabilities() Caps { return Caps{Walk: true} }
func (slowConn) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }
func (s slowConn) Walk(ctx context.Context, _ SourceSpec, emit func(Document) error) error {
	select {
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
	}
	_ = emit(Document{ID: "x", Text: "y"})
	atomic.AddInt32(s.done, 1)
	return nil
}

// TestService_Shutdown_Drains proves Shutdown waits for an in-flight sync to
// finish and then refuses new work.
func TestService_Shutdown_Drains(t *testing.T) {
	var done int32
	Register(slowConn{done: &done})
	svc := NewService(NewMemCursor(), 4)
	sink := &countSink{}
	svc.Register(SourceSpec{Name: "s", Type: "slow", KB: "kb", Triggers: []Trigger{{Type: "on_start"}}}, sink)

	time.Sleep(20 * time.Millisecond) // let the on_start job enter Walk
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc.Shutdown(ctx)

	if atomic.LoadInt32(&done) != 1 {
		t.Fatalf("Shutdown did not drain in-flight sync (done=%d)", done)
	}
	// After shutdown, new registers must not run.
	before := atomic.LoadInt64(&sink.ups)
	svc.Register(SourceSpec{Name: "after", Type: "slow", KB: "kb", Triggers: []Trigger{{Type: "on_start"}}}, sink)
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt64(&sink.ups) != before {
		t.Fatalf("a job ran after Shutdown")
	}
}
