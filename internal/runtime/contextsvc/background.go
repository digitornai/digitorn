package contextsvc

import (
	"context"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/safego"
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

// recompute does the EXACT count for one session via the tokenizer worker.
// Fully recover-guarded : a panic in the counter / view can never crash the
// pool. On any error (worker down/slow) it returns WITHOUT calling onResult, so
// the gauge keeps its last exact value — never an estimate.
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
	// Count each budget bucket EXACTLY. Any per-bucket error aborts the whole
	// recompute (no estimate, no partial gauge) — the last exact value stays.
	sys, err := b.count(ctx, v.System, v.Provider, v.Model)
	if err != nil {
		return
	}
	tools, err := b.count(ctx, v.Tools, v.Provider, v.Model)
	if err != nil {
		return
	}
	msgs, err := b.count(ctx, v.Messages, v.Provider, v.Model)
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

// count returns the exact token count of a bucket (0 for an empty bucket, with
// no RPC).
func (b *Background) count(ctx context.Context, texts []string, provider, model string) (int, error) {
	if len(texts) == 0 {
		return 0, nil
	}
	return b.counter.CountTotal(ctx, texts, provider, model)
}
