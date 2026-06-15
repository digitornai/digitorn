package rag

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// lockedBackend wraps fakeBackend with a mutex so the TEST FAKE's own maps are
// not the source of any race — isolating the engine's kbIndex.docs race.
type lockedBackend struct {
	mu sync.Mutex
	b  *fakeBackend
}

func newLockedBackend() *lockedBackend { return &lockedBackend{b: newFakeBackend()} }

func (l *lockedBackend) EnsureKB(ctx context.Context, kb string, dim int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.EnsureKB(ctx, kb, dim)
}
func (l *lockedBackend) DeleteKB(ctx context.Context, kb string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.DeleteKB(ctx, kb)
}
func (l *lockedBackend) ListKBs(ctx context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.ListKBs(ctx)
}
func (l *lockedBackend) CountKB(ctx context.Context, kb string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.CountKB(ctx, kb)
}
func (l *lockedBackend) KBInfo(ctx context.Context, kb string) (KBStats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.KBInfo(ctx, kb)
}
func (l *lockedBackend) Upsert(ctx context.Context, kb string, docs []Document) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Upsert(ctx, kb, docs)
}
func (l *lockedBackend) Search(ctx context.Context, kb string, vec []float32, topK int, filter Filter) ([]SearchHit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Search(ctx, kb, vec, topK, filter)
}
func (l *lockedBackend) DeleteBySource(ctx context.Context, kb, source string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.DeleteBySource(ctx, kb, source)
}
func (l *lockedBackend) Scan(ctx context.Context, kb string) ([]Document, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Scan(ctx, kb)
}
func (l *lockedBackend) Close() error { return nil }

// TestChaos_ConcurrentIngestQuery_Race exercises concurrent Ingest + Query on
// the SAME engine/kb to surface the data race on kbIndex.docs (a plain map
// written by ix.add() in IngestWithMeta — OUTSIDE e.mu, since indexFor releases
// the lock before add — and read by bm25Search/hybridSearchVec also outside
// e.mu). The BM25 index has its own mutex, but kbIndex.docs does not.
//
// Run with -race:
//
//	go test ./internal/modules/rag/ -run TestChaos_ConcurrentIngestQuery_Race -race -count=1
func TestChaos_ConcurrentIngestQuery_Race(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{
		"pipeline": map[string]any{"retrieval": "hybrid"},
	})
	eng := NewEngine(cfg, newLockedBackend(), fakeEmbedder{dim: 64}, nil)
	ctx := context.Background()

	// Seed so the KB exists and a first query path is valid.
	if _, err := eng.Ingest(ctx, "kb", "seed alpha beta gamma delta epsilon", "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: continuously ingest new sources (each ix.add writes ix.docs).
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				src := fmt.Sprintf("w%d-doc%d", w, i)
				_, _ = eng.Ingest(ctx, "kb",
					fmt.Sprintf("déploiement application serveur production %d alpha beta", i), src)
				i++
			}
		}(w)
	}

	// Readers: continuously query (bm25Search + hybridSearchVec read ix.docs).
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = eng.Query(ctx, "kb", "comment déployer application serveur", 5)
			}
		}()
	}

	// Let them hammer for a bit; the race detector flags the unsynchronized
	// map access the moment it interleaves.
	const iters = 400
	for i := 0; i < iters; i++ {
		_, _ = eng.Query(ctx, "kb", "alpha beta gamma", 3)
	}
	close(stop)
	wg.Wait()
	t.Log("if -race printed a WARNING: DATA RACE above, the kbIndex.docs map is concurrently read/written without synchronization (engine.go:42 write vs engine.go:265/309 read, both outside e.mu).")
}
