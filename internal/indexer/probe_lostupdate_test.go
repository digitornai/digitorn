package indexer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// probeSink counts EVERY upsert call per doc id (not dedup by map key), so we
// can detect double-embedding: an id upserted twice => embedded twice.
type probeSink struct {
	mu      sync.Mutex
	perID   map[string]int
	total   int64
	embeds  int64 // simulate embed cost: one per doc in each Upsert batch
}

func newProbeSink() *probeSink { return &probeSink{perID: map[string]int{}} }

func (s *probeSink) Upsert(_ context.Context, _ string, docs []Document) error {
	s.mu.Lock()
	for _, d := range docs {
		s.perID[d.ID]++
		atomic.AddInt64(&s.total, 1)
		atomic.AddInt64(&s.embeds, 1)
	}
	s.mu.Unlock()
	return nil
}
func (s *probeSink) Delete(_ context.Context, _, id string) error { return nil }

// staticConn emits N fixed docs every Walk.
type staticConn struct {
	typ  string
	docs []Document
}

func (c *staticConn) Type() string      { return c.typ }
func (c *staticConn) Capabilities() Caps { return Caps{Walk: true} }
func (c *staticConn) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }
func (c *staticConn) Walk(_ context.Context, _ SourceSpec, emit func(Document) error) error {
	for _, d := range c.docs {
		if err := emit(d); err != nil {
			return err
		}
	}
	return nil
}

// TestDirectSync_SingleFlight_NoDoubleIndex proves the per-source in-process
// lock makes two concurrent direct Sync() of the SAME source safe : the second
// blocks until the first saves the cursor, then sees no changes — so each doc
// is upserted (and embedded) exactly once, never double-billed. Regression for
// the lost-update the indexer probe found on the unleased direct-Sync path.
func TestDirectSync_SingleFlight_NoDoubleIndex(t *testing.T) {
	const n = 50
	docs := make([]Document, n)
	for i := range docs {
		docs[i] = Document{ID: fmt.Sprintf("doc-%02d", i), Text: fmt.Sprintf("body number %d", i)}
	}
	conn := &staticConn{typ: "probe-lostupdate", docs: docs}
	Register(conn)

	svc := NewService(NewMemCursor(), 8)
	sink := newProbeSink()
	spec := SourceSpec{Name: "s", Type: "probe-lostupdate", KB: "kb"}
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Sync(ctx, spec, sink); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	total := atomic.LoadInt64(&sink.total)
	doubled := 0
	for _, c := range sink.perID {
		if c > 1 {
			doubled++
		}
	}
	t.Logf("total upserts=%d across 2 concurrent unleased Sync() of one source (%d docs)", total, n)
	t.Logf("%d/%d docs double-embedded", doubled, n)
	t.Logf("embed calls=%d (cost: with a real embedder this is the embedding bill)", atomic.LoadInt64(&sink.embeds))

	if total != n {
		t.Errorf("LOST UPDATE: expected exactly %d upserts (each doc once), got %d", n, total)
	}
	if doubled != 0 {
		t.Errorf("LOST UPDATE: %d/%d docs were embedded/upserted MORE THAN ONCE", doubled, n)
	}
}
