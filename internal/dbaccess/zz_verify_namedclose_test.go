package dbaccess

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type vncDB struct {
	id     int64
	closed atomic.Bool
}

func (d *vncDB) Kind() string { return "vnc" }
func (d *vncDB) Query(ctx context.Context, q string, a ...any) (*Result, error) {
	if d.closed.Load() {
		return nil, vncErr("vnc: closed")
	}
	return &Result{Rows: []Row{{"ok": 1}}, RowCount: 1}, nil
}
func (d *vncDB) Schema(ctx context.Context) (*Catalog, error) { return &Catalog{}, nil }
func (d *vncDB) Close() error                                 { d.closed.Store(true); return nil }

type vncErr string

func (e vncErr) Error() string { return string(e) }

var vncOpens atomic.Int64

func TestVerify_NamedConcurrentClosedHandles(t *testing.T) {
	vncOpens.Store(0)
	Register("vnc", func(ctx context.Context, cfg ConnConfig) (DB, error) {
		vncOpens.Add(1)
		time.Sleep(20 * time.Millisecond) // slow dial
		return &vncDB{id: vncOpens.Load()}, nil
	})

	mgr := NewManager(256, 30*time.Minute)
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "shared", Kind: "vnc"}

	const n = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	handles := make([]DB, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			handles[i], errs[i] = mgr.Named(context.Background(), "app", cfg)
		}(i)
	}
	close(start)
	wg.Wait()

	closed, usable, errCount := 0, 0, 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			errCount++
			continue
		}
		if handles[i].(*vncDB).closed.Load() {
			closed++
		} else {
			usable++
		}
	}
	t.Logf("opens=%d  usable=%d  ALREADY-CLOSED=%d  errs=%d  (of n=%d)", vncOpens.Load(), usable, closed, errCount, n)

	// Now simulate a real downstream Query on each returned handle, like the
	// agent `query` tool would do AFTER receiving the handle from Named.
	failedQuery := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			continue
		}
		if _, qerr := handles[i].Query(context.Background(), "SELECT 1"); qerr != nil {
			failedQuery++
		}
	}
	t.Logf("downstream Query failures on returned handles: %d of %d", failedQuery, n)

	// Verify the pooled entry (what a SECOND Named call would get) is alive.
	pooled, _ := mgr.Named(context.Background(), "app", cfg)
	t.Logf("pooled entry alive after race: %v (opens now=%d)", !pooled.(*vncDB).closed.Load(), vncOpens.Load())
}
