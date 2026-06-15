package dbaccess

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type advRaceDB struct {
	id     int64
	closed atomic.Bool
}

func (d *advRaceDB) Kind() string { return "advrace" }
func (d *advRaceDB) Query(ctx context.Context, q string, args ...any) (*Result, error) {
	if d.closed.Load() {
		return nil, advRaceClosedErr
	}
	return &Result{Rows: []Row{{"ok": 1}}, RowCount: 1}, nil
}
func (d *advRaceDB) Schema(ctx context.Context) (*Catalog, error) { return &Catalog{}, nil }
func (d *advRaceDB) Close() error { d.closed.Store(true); return nil }

type advRaceErr string

func (e advRaceErr) Error() string { return string(e) }

const advRaceClosedErr = advRaceErr("advrace: database is closed")

var advRaceOpens atomic.Int64

func TestAdv_NamedRace_ClosedHandles(t *testing.T) {
	advRaceOpens.Store(0)
	Register("advrace", func(ctx context.Context, cfg ConnConfig) (DB, error) {
		n := advRaceOpens.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &advRaceDB{id: n}, nil
	})

	mgr := NewManager(256, 30*time.Minute)
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "shared", Kind: "advrace"}

	const n = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	handles := make([]DB, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			db, err := mgr.Named(context.Background(), "raceApp", cfg)
			if err == nil {
				handles[i] = db
			}
		}(i)
	}
	close(start)
	wg.Wait()

	closedHandles, usable := 0, 0
	for i := 0; i < n; i++ {
		if handles[i] == nil {
			continue
		}
		if handles[i].(*advRaceDB).closed.Load() {
			closedHandles++
		} else {
			usable++
		}
	}
	t.Logf("opens=%d (single-flight would give 1)", advRaceOpens.Load())
	t.Logf("returned handles: usable=%d ALREADY-CLOSED=%d of %d", usable, closedHandles, n)
	if advRaceOpens.Load() <= 1 {
		t.Fatalf("race did not trigger: opens=%d", advRaceOpens.Load())
	}
}
