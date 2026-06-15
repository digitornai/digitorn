package indexer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// lockingCursor is a Cursor that ALSO implements Locker with REAL mutual
// exclusion (a single in-proc mutex per key) — i.e. a correct lease backend.
// It models the production case where a Locker IS configured.
type lockingCursor struct {
	mu    sync.Mutex
	store map[string][]byte
	locks sync.Map // key -> *sync.Mutex
}

func newLockingCursor() *lockingCursor { return &lockingCursor{store: map[string][]byte{}} }

func (c *lockingCursor) Load(key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store[key], nil
}
func (c *lockingCursor) Save(key string, b []byte) error {
	c.mu.Lock()
	c.store[key] = b
	c.mu.Unlock()
	return nil
}

// Acquire blocks until it holds the per-key lease (real exclusion), matching
// the Locker contract the scheduled path relies on.
func (c *lockingCursor) Acquire(_ context.Context, key string) (func(), bool) {
	mAny, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	m := mAny.(*sync.Mutex)
	m.Lock()
	return func() { m.Unlock() }, true
}

func TestProbe_ManualReindexBypassesLease(t *testing.T) {
	const n = 50
	docs := make([]Document, n)
	for i := range docs {
		docs[i] = Document{ID: fmt.Sprintf("d-%02d", i), Text: fmt.Sprintf("v %d", i)}
	}
	conn := &staticConn{typ: "probe-leasebypass", docs: docs}
	Register(conn)

	cur := newLockingCursor()
	svc := NewService(cur, 8)
	sink := newProbeSink()
	spec := SourceSpec{Name: "s", Type: "probe-leasebypass", KB: "kb"}
	key := stateKey(spec)
	ctx := context.Background()

	start := make(chan struct{})
	var wg sync.WaitGroup

	// G1 = the SCHEDULED tick: acquires the lease exactly as service.go:218-226
	// does inside Register's syncFn, then syncs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		release, ok := cur.Acquire(ctx, key)
		if !ok {
			return
		}
		// hold the lease across the whole sync, like the scheduled path
		defer release()
		_, _ = svc.Sync(ctx, spec, sink)
	}()

	// G2 = the MANUAL reindex (module.go:535): calls Sync directly, never
	// acquires the lease G1 holds. Both start together to force the overlap.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, _ = svc.Sync(ctx, spec, sink)
	}()

	close(start)
	wg.Wait()

	total := atomic.LoadInt64(&sink.total)
	doubled := 0
	for _, c := range sink.perID {
		if c > 1 {
			doubled++
		}
	}
	t.Logf("with a REAL Locker held by the scheduled tick: total upserts=%d (%d docs)", total, n)
	t.Logf("%d/%d docs double-embedded — manual Sync ignored the held lease", doubled, n)
	if total > n {
		t.Logf("LEASE BYPASS CONFIRMED: manual Sync double-embedded %d docs despite a held lease (total %d > %d)", doubled, total, n)
	}
}
