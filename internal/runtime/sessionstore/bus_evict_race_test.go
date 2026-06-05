package sessionstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBus_EvictDuringAppend_NoSeqCorruption hammers a single session with
// concurrent appends while a second goroutine evicts that same session in a
// tight loop. Before the validated-lock fix, eviction deleted the session lock
// without holding it, so a new append could create a fresh lock and run
// concurrently with an in-flight append — racing the seq allocator into
// duplicate / skipped seqs. Run with -race ; assert the persisted log has every
// successful append exactly once, with no duplicates and no gaps.
func TestBus_EvictDuringAppend_NoSeqCorruption(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()

	const sid = "race-sess"
	const writers = 8
	const perWriter = 200

	var ok, failed atomic.Int64
	var writerWg sync.WaitGroup
	var evictWg sync.WaitGroup
	stop := make(chan struct{})

	// Evictor : drop the session (state + lock + seq allocator) as fast as it can,
	// exactly the path that used to race appends.
	evictWg.Add(1)
	go func() {
		defer evictWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				bus.evictLocked(sid, true)
			}
		}
	}()

	for w := 0; w < writers; w++ {
		writerWg.Add(1)
		go func(w int) {
			defer writerWg.Done()
			for j := 0; j < perWriter; j++ {
				if _, err := bus.Append(context.Background(), makeUserMsg(sid, fmt.Sprintf("%d-%d", w, j))); err != nil {
					failed.Add(1)
				} else {
					ok.Add(1)
				}
			}
		}(w)
	}

	writerWg.Wait()
	close(stop)
	evictWg.Wait()

	if failed.Load() != 0 {
		t.Fatalf("appends failed: %d (expected 0 with an unbounded queue)", failed.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	res, err := ReadJSONL(flusher.cfg.Paths.EventsFile(sid), JSONLBestEffort, "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	seqs := make([]uint64, len(res.Events))
	for i := range res.Events {
		seqs[i] = res.Events[i].Seq
	}
	sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
	seen := map[uint64]bool{}
	for _, s := range seqs {
		if seen[s] {
			t.Fatalf("duplicate seq %d on disk — eviction raced an append", s)
		}
		seen[s] = true
	}
	// Every successful append is persisted exactly once, and the seqs form a
	// contiguous 1..N run (no gaps from a reissued / skipped allocation).
	if uint64(len(seqs)) != uint64(ok.Load()) {
		t.Fatalf("persisted %d events, expected %d successful appends", len(seqs), ok.Load())
	}
	if len(seqs) > 0 && (seqs[0] != 1 || seqs[len(seqs)-1] != uint64(len(seqs))) {
		t.Fatalf("seqs not contiguous 1..%d : first=%d last=%d", len(seqs), seqs[0], seqs[len(seqs)-1])
	}
}
