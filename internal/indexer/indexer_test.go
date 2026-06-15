package indexer

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeConn struct {
	mu   sync.Mutex
	docs []Document
}

func (f *fakeConn) Type() string      { return "fake" }
func (f *fakeConn) Capabilities() Caps { return Caps{Walk: true} }
func (f *fakeConn) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }
func (f *fakeConn) Walk(_ context.Context, _ SourceSpec, emit func(Document) error) error {
	f.mu.Lock()
	docs := append([]Document(nil), f.docs...)
	f.mu.Unlock()
	for _, d := range docs {
		if err := emit(d); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeConn) set(d []Document) { f.mu.Lock(); f.docs = d; f.mu.Unlock() }

type fakeSink struct {
	mu      sync.Mutex
	upserts map[string]Document
	deletes []string
}

func newFakeSink() *fakeSink { return &fakeSink{upserts: map[string]Document{}} }
func (s *fakeSink) Upsert(_ context.Context, _ string, docs []Document) error {
	s.mu.Lock()
	for _, d := range docs {
		s.upserts[d.ID] = d
	}
	s.mu.Unlock()
	return nil
}
func (s *fakeSink) Delete(_ context.Context, _, id string) error {
	s.mu.Lock()
	s.deletes = append(s.deletes, id)
	delete(s.upserts, id)
	s.mu.Unlock()
	return nil
}
func (s *fakeSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.upserts) }

func TestService_IncrementalSync(t *testing.T) {
	conn := &fakeConn{docs: []Document{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}}}
	Register(conn)
	svc := NewService(nil, 4)
	sink := newFakeSink()
	spec := SourceSpec{Name: "s", Type: "fake", KB: "kb"}
	ctx := context.Background()

	rep, err := svc.Sync(ctx, spec, sink)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 2 || sink.count() != 2 {
		t.Fatalf("first sync: rep=%+v upserts=%d, want 2 added", rep, sink.count())
	}

	if rep, _ = svc.Sync(ctx, spec, sink); rep.Added+rep.Updated+rep.Deleted != 0 {
		t.Errorf("idempotent re-sync changed: %+v", rep)
	}

	conn.set([]Document{{ID: "b", Text: "beta v2"}, {ID: "c", Text: "gamma"}})
	rep, _ = svc.Sync(ctx, spec, sink)
	if rep.Updated != 1 || rep.Added != 1 || rep.Deleted != 1 {
		t.Errorf("after change rep=%+v, want updated=1 added=1 deleted=1", rep)
	}
	if _, gone := sink.upserts["a"]; gone {
		t.Error("removed doc 'a' still present in sink")
	}
}

func TestService_OnStartTrigger(t *testing.T) {
	conn := &fakeConn{docs: []Document{{ID: "x", Text: "one"}}}
	Register(conn)
	svc := NewService(nil, 4)
	sink := newFakeSink()
	spec := SourceSpec{Name: "ostart", Type: "fake", KB: "kb", Triggers: []Trigger{{Type: "on_start"}}}

	svc.Register(spec, sink)
	ok := false
	for i := 0; i < 50; i++ {
		if sink.count() == 1 {
			ok = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("on_start trigger never synced the source")
	}
	svc.Deregister(spec)
}
