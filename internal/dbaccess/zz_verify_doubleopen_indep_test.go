package dbaccess

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// independent fake DB: each Open returns a distinct handle that records whether
// it has been Closed. Query fails iff the handle was Closed. This makes
// "already-closed handle returned to caller" unambiguous (no reliance on
// *sql.DB "database is closed" string).
type fakeDB struct {
	id     int
	closed atomic.Bool
}

func (f *fakeDB) Kind() string { return "fake" }
func (f *fakeDB) Query(ctx context.Context, q string, args ...any) (*Result, error) {
	if f.closed.Load() {
		return nil, fmt.Errorf("fake handle %d: database is closed", f.id)
	}
	return &Result{Rows: []Row{}}, nil
}
func (f *fakeDB) Schema(ctx context.Context) (*Catalog, error) { return &Catalog{}, nil }
func (f *fakeDB) Close() error {
	f.closed.Store(true)
	return nil
}

var fakeOpenCount atomic.Int64

func init() {
	Register("fakeio", func(ctx context.Context, cfg ConnConfig) (DB, error) {
		// Small artificial delay + force overlap so multiple goroutines are
		// inside Open() before any reaches store(). This is the realistic
		// first-use stampede the bug describes.
		n := fakeOpenCount.Add(1)
		time.Sleep(15 * time.Millisecond)
		return &fakeDB{id: int(n)}, nil
	})
}

func TestVerify_IndepDoubleOpen_Fake(t *testing.T) {
	mgr := NewManager(256, 30*time.Minute)
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "shared", Kind: "fakeio"}
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	handles := make([]DB, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once -> maximize first-use overlap
			db, err := mgr.Named(ctx, "raceApp", cfg)
			handles[i], errs[i] = db, err
		}(i)
	}
	close(start)
	wg.Wait()

	opens := fakeOpenCount.Load()
	var usable, closed, openErr int
	for i := 0; i < n; i++ {
		if errs[i] != nil || handles[i] == nil {
			openErr++
			continue
		}
		if _, err := handles[i].Query(ctx, "SELECT 1"); err != nil {
			closed++
		} else {
			usable++
		}
	}
	t.Logf("physical Open() calls = %d (expected 1 if single-flight, ~%d if double-open)", opens, n)
	t.Logf("returned handles: usable=%d closed=%d openErr=%d (of %d)", usable, closed, openErr, n)

	// Independent assertions of the CLAIM:
	if opens <= 1 {
		t.Fatalf("CLAIM REFUTED: only %d physical opens -> single-flight present", opens)
	}
	if closed == 0 {
		t.Logf("WARNING: no closed handle this run; race did not interleave. opens=%d", opens)
	}
}
