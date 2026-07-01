package contextsvc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

// TokenCounter counts tokens EXACTLY for a batch of texts under a model.
// Satisfied by *tokenizer.Client (the out-of-process tokenizer worker). There
// is no estimate path : production occupancy is the real count.
type TokenCounter interface {
	CountTotal(ctx context.Context, texts []string, provider, model string) (int, error)
}

// View is the EXACT context the model sees for a session, split into the three
// budget buckets, plus the brain for tokenizer routing. system = the assembled
// system prompt ; tools = the injected tool schemas ; messages = the (compacted)
// conversation. Their token sum is the real prompt size.
type View struct {
	System   []string
	Tools    []string
	Messages []string
	Provider string
	Model    string
}

// ViewSource resolves the current context view for a session. ok=false → skip
// (no session, nothing to count).
type ViewSource interface {
	ContextView(sessionID string) (View, bool)
}

// Result is the EXACT breakdown handed back to the runtime. The runtime sets
// the gauge to Total, streams the split to clients, and exposes it as a turn
// variable — none of which the recompute blocks on.
type Result struct {
	SessionID string
	System    int
	Tools     int
	Messages  int
	Total     int
}

// Background recomputes the EXACT context token count OFF the turn loop. The
// runtime calls Touch (non-blocking) on every context-changing event ; a pool
// of goroutines coalesces per session, counts via the tokenizer worker, and
// hands the exact number to onResult. NOTHING the runtime does ever blocks on
// this : Touch never blocks, the worker RPC happens only here, and a worker
// failure simply leaves the last exact value in place (never an estimate).
type Background struct {
	counter  TokenCounter
	view     ViewSource
	onResult func(Result)
	timeout  time.Duration

	mu      sync.Mutex
	pending map[string]struct{}
	queue   chan string
	stop    chan struct{}
	wg      sync.WaitGroup
	started bool
}

// NewBackground constructs the service. queueCap bounds the coalescing buffer ;
// onResult is called from a worker goroutine with the exact count.
func NewBackground(counter TokenCounter, view ViewSource, onResult func(Result)) *Background {
	return &Background{
		counter:  counter,
		view:     view,
		onResult: onResult,
		timeout:  5 * time.Second,
		pending:  make(map[string]struct{}),
		queue:    make(chan string, 4096),
		stop:     make(chan struct{}),
	}
}

// Start spawns the worker pool. Safe to call once. workers<=0 → a sensible
// default.
func (b *Background) Start(workers int) {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.mu.Unlock()
	if workers <= 0 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		safego.Go("contextsvc.background", func() {
			defer b.wg.Done()
			b.loop()
		})
	}
}

// Stop signals the pool and waits for in-flight recomputes to drain.
func (b *Background) Stop() {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return
	}
	b.started = false
	close(b.stop)
	b.mu.Unlock()
	b.wg.Wait()
}

// Touch signals that a session's context changed and should be recounted. It
// NEVER blocks : a duplicate pending touch is dropped (coalesced), and a full
// queue drops the marker so a later Touch re-enqueues. The runtime calls this
// on every message / tool_call / tool_result / turn boundary.
func (b *Background) Touch(sessionID string) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	if _, dup := b.pending[sessionID]; dup {
		b.mu.Unlock()
		return
	}
	b.pending[sessionID] = struct{}{}
	b.mu.Unlock()

	select {
	case b.queue <- sessionID:
	default:
		// Queue saturated : drop the marker (never block the caller). The next
		// Touch for this session re-enqueues, so no recount is permanently lost.
		b.mu.Lock()
		delete(b.pending, sessionID)
		b.mu.Unlock()
	}
}

func (b *Background) loop() {
	for {
		select {
		case <-b.stop:
			return
		case sid := <-b.queue:
			b.mu.Lock()
			delete(b.pending, sid)
			b.mu.Unlock()
			b.recompute(sid)
		}
	}
}

// Recompute runs an exact synchronous token count for a session and calls
// onResult with the result. Blocks until the count completes or times out.
// Use after compaction to get the real post-compaction context immediately.
func (b *Background) Recompute(sid string) {
	b.recompute(sid)
}

func (b *Background) recompute(sid string) {
	defer safego.Recover("contextsvc.recompute")
	if b.view == nil || b.counter == nil {
		return
	}
	v, ok := b.view.ContextView(sid)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	// Count each budget bucket EXACTLY. Any error aborts the whole recompute (no
	// estimate, no partial gauge) — the last exact value stays.
	sys, tools, msgs, err := b.countBuckets(ctx, v)
	if err != nil {
		return
	}
	total := sys + tools + msgs
	if total <= 0 {
		return
	}
	if b.onResult != nil {
		b.onResult(Result{SessionID: sid, System: sys, Tools: tools, Messages: msgs, Total: total})
	}
}

// batchCounter is an optional TokenCounter that counts many texts in ONE pass,
// returning per-text counts. *tokenizer.Client implements it.
type batchCounter interface {
	CountEach(ctx context.Context, texts []string, provider, model string) ([]int, error)
}

// countBuckets counts the three budget buckets. When the counter supports batch
// per-text counting (CTX-8 Phase 4) it does ONE RPC pass over all texts and
// splits the result by bucket boundary — fewer round-trips on the background
// pool. Otherwise it falls back to one CountTotal per bucket (unchanged).
func (b *Background) countBuckets(ctx context.Context, v View) (sys, tools, msgs int, err error) {
	if bc, ok := b.counter.(batchCounter); ok {
		return b.countBucketsBatched(ctx, bc, v)
	}
	if sys, err = b.count(ctx, v.System, v.Provider, v.Model); err != nil {
		return
	}
	if tools, err = b.count(ctx, v.Tools, v.Provider, v.Model); err != nil {
		return
	}
	if msgs, err = b.count(ctx, v.Messages, v.Provider, v.Model); err != nil {
		return
	}
	return sys, tools, msgs, nil
}

func (b *Background) countBucketsBatched(ctx context.Context, bc batchCounter, v View) (sys, tools, msgs int, err error) {
	all := make([]string, 0, len(v.System)+len(v.Tools)+len(v.Messages))
	all = append(all, v.System...)
	all = append(all, v.Tools...)
	all = append(all, v.Messages...)
	if len(all) == 0 {
		return 0, 0, 0, nil
	}
	counts, err := bc.CountEach(ctx, all, v.Provider, v.Model)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(counts) != len(all) {
		return 0, 0, 0, fmt.Errorf("contextsvc: per-text count length mismatch (%d vs %d)", len(counts), len(all))
	}
	i := 0
	for range v.System {
		sys += counts[i]
		i++
	}
	for range v.Tools {
		tools += counts[i]
		i++
	}
	for range v.Messages {
		msgs += counts[i]
		i++
	}
	return sys, tools, msgs, nil
}

// count returns the exact token count of a bucket (0 for an empty bucket, with
// no RPC).
func (b *Background) count(ctx context.Context, texts []string, provider, model string) (int, error) {
	if len(texts) == 0 {
		return 0, nil
	}
	return b.counter.CountTotal(ctx, texts, provider, model)
}
