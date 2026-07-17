package contextsvc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

type TokenCounter interface {
	CountTotal(ctx context.Context, texts []string, provider, model string) (int, error)
}

type View struct {
	System   []string
	Tools    []string
	Messages []string
	Provider string
	Model    string
}

type ViewSource interface {
	ContextView(sessionID string) (View, bool)
}

type Result struct {
	SessionID string
	System    int
	Tools     int
	Messages  int
	Total     int
}

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

type batchCounter interface {
	CountEach(ctx context.Context, texts []string, provider, model string) ([]int, error)
}

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

func (b *Background) count(ctx context.Context, texts []string, provider, model string) (int, error) {
	if len(texts) == 0 {
		return 0, nil
	}
	return b.counter.CountTotal(ctx, texts, provider, model)
}
