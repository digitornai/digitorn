package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

var (
	ErrNoSessionID       = errors.New("bus: event has empty session_id")
	ErrBusStopped        = errors.New("bus: stopped")
	ErrEmptySIDSubscribe = errors.New("bus: Subscribe requires a non-empty session_id; use SubscribeAll for the bridge-only global subscription")
	ErrNilCallback       = errors.New("bus: nil callback")
	ErrBusNotStarted     = errors.New("bus: not started; call Start first")
)

type BusConfig struct {
	Paths   Paths
	Flusher *DiskFlusher
	Seqs    *SeqRegistry
	Now     func() time.Time
	Logger  *slog.Logger

	SubscriberQueueSize    int
	SubscriberMaxSlowDrops uint64

	MaxStatesInMemory   int
	StateIdleEvictAfter time.Duration
	EvictionInterval    time.Duration
}

type Bus struct {
	cfg BusConfig
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	states      sync.Map
	lastTouch   sync.Map
	statesCount atomic.Int64

	sessionLocks sync.Map

	subsMu     sync.RWMutex
	subsPerSID map[string][]*subscription
	subsAll    []*subscription
	nextSubID  atomic.Int64

	appendTotal     atomic.Uint64
	appendErrors    atomic.Uint64
	dropped         atomic.Uint64
	notifyTotal     atomic.Uint64
	callbackPanics  atomic.Uint64
	subscriberDrops atomic.Uint64
	subscriberKicks atomic.Uint64
	statesEvicted   atomic.Uint64

	started atomic.Bool
	stopped atomic.Bool
}

type unboundedQueue struct {
	mu     sync.Mutex
	items  []Event
	signal chan struct{}
	done   chan struct{}
}

func newUnboundedQueue() *unboundedQueue {
	return &unboundedQueue{
		signal: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (q *unboundedQueue) push(ev Event) {
	q.mu.Lock()
	q.items = append(q.items, ev)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *unboundedQueue) run(ctx context.Context, cb func(Event)) {
	for {
		select {
		case <-q.done:
			return
		case <-ctx.Done():
			return
		case <-q.signal:
			for {
				q.mu.Lock()
				if len(q.items) == 0 {
					q.mu.Unlock()
					break
				}
				ev := q.items[0]
				q.items = q.items[1:]
				q.mu.Unlock()
				cb(ev)
			}
		}
	}
}

func (q *unboundedQueue) stop() {
	close(q.done)
}

type subscription struct {
	bus   *Bus
	id    int64
	sid   string
	cb    func(Event)
	uq    *unboundedQueue
	done  chan struct{}

	panics atomic.Uint64
	closed atomic.Bool
}

type Subscription struct {
	bus *Bus
	sub *subscription
}

func (s *Subscription) Cancel() {
	if s == nil || s.bus == nil || s.sub == nil {
		return
	}
	s.bus.cancelSubscription(s.sub)
}

func (s *Subscription) Stats() (drops, panics uint64) {
	if s == nil || s.sub == nil {
		return 0, 0
	}
	return 0, s.sub.panics.Load()
}

func NewBus(cfg BusConfig) (*Bus, error) {
	if cfg.Paths.Root == "" {
		return nil, errors.New("bus: empty paths root")
	}
	if cfg.Flusher == nil {
		return nil, errors.New("bus: flusher is required")
	}
	if cfg.Seqs == nil {
		cfg.Seqs = NewSeqRegistry(cfg.Paths)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.SubscriberQueueSize <= 0 {
		cfg.SubscriberQueueSize = 1024
	}
	if cfg.SubscriberMaxSlowDrops == 0 {
		cfg.SubscriberMaxSlowDrops = 100
	}
	if cfg.MaxStatesInMemory <= 0 {
		cfg.MaxStatesInMemory = 100_000
	}
	if cfg.StateIdleEvictAfter <= 0 {
		cfg.StateIdleEvictAfter = 30 * time.Minute
	}
	if cfg.EvictionInterval <= 0 {
		cfg.EvictionInterval = 1 * time.Minute
	}

	return &Bus{
		cfg:        cfg,
		log:        cfg.Logger,
		subsPerSID: map[string][]*subscription{},
	}, nil
}

func (b *Bus) Start(parent context.Context) error {
	if !b.started.CompareAndSwap(false, true) {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	b.ctx, b.cancel = context.WithCancel(parent)

	b.wg.Add(1)
	go b.evictionLoop()

	return nil
}

func (b *Bus) Stop(ctx context.Context) error {
	if !b.started.Load() {
		b.stopped.Store(true)
		return nil
	}
	if !b.stopped.CompareAndSwap(false, true) {
		return nil
	}

	b.cancel()

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bus) Append(ctx context.Context, ev Event) (uint64, error) {
	if b.stopped.Load() {
		return 0, ErrBusStopped
	}
	if !b.started.Load() {
		return 0, ErrBusNotStarted
	}
	if ev.SessionID == "" {
		return 0, ErrNoSessionID
	}
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = b.cfg.Now().UnixNano()
	}
	if ev.Type == "" {
		return 0, errors.New("bus: event type required")
	}

	lock := b.lockSessionValidated(ev.SessionID)
	defer lock.Unlock()

	alloc, err := b.cfg.Seqs.For(ev.SessionID)
	if err != nil {
		b.appendErrors.Add(1)
		return 0, fmt.Errorf("bus: seq allocator: %w", err)
	}
	tentativeSeq := alloc.Current() + 1
	ev.Seq = tentativeSeq

	if err := b.cfg.Flusher.Enqueue(ev); err != nil {
		b.appendErrors.Add(1)
		if errors.Is(err, ErrQueueFull) {
			b.dropped.Add(1)
		}
		return 0, fmt.Errorf("bus: enqueue: %w", err)
	}

	finalSeq := alloc.Next()
	if finalSeq != tentativeSeq {
		return 0, fmt.Errorf("bus: seq race (tentative=%d final=%d)",
			tentativeSeq, finalSeq)
	}

	state := b.stateForLocked(ev.SessionID)
	Apply(state, &ev)
	b.touchSession(ev.SessionID)

	b.notify(ev)
	b.appendTotal.Add(1)
	return ev.Seq, nil
}

func (b *Bus) AppendBlocking(ctx context.Context, ev Event) (uint64, error) {
	if b.stopped.Load() {
		return 0, ErrBusStopped
	}
	if !b.started.Load() {
		return 0, ErrBusNotStarted
	}
	if ev.SessionID == "" {
		return 0, ErrNoSessionID
	}
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = b.cfg.Now().UnixNano()
	}
	if ev.Type == "" {
		return 0, errors.New("bus: event type required")
	}

	lock := b.lockSessionValidated(ev.SessionID)
	defer lock.Unlock()

	alloc, err := b.cfg.Seqs.For(ev.SessionID)
	if err != nil {
		b.appendErrors.Add(1)
		return 0, fmt.Errorf("bus: seq allocator: %w", err)
	}
	tentativeSeq := alloc.Current() + 1
	ev.Seq = tentativeSeq

	if err := b.cfg.Flusher.EnqueueBlocking(ctx, ev); err != nil {
		b.appendErrors.Add(1)
		if errors.Is(err, ErrQueueFull) {
			b.dropped.Add(1)
		}
		return 0, fmt.Errorf("bus: enqueue: %w", err)
	}

	finalSeq := alloc.Next()
	if finalSeq != tentativeSeq {
		return 0, fmt.Errorf("bus: seq race (tentative=%d final=%d)",
			tentativeSeq, finalSeq)
	}

	state := b.stateForLocked(ev.SessionID)
	Apply(state, &ev)
	b.touchSession(ev.SessionID)

	b.notify(ev)
	b.appendTotal.Add(1)
	return ev.Seq, nil
}

func (b *Bus) AppendDurableBatch(ctx context.Context, evs []Event) ([]uint64, error) {
	if b.stopped.Load() {
		return nil, ErrBusStopped
	}
	if !b.started.Load() {
		return nil, ErrBusNotStarted
	}
	if len(evs) == 0 {
		return nil, nil
	}
	sid := evs[0].SessionID
	if sid == "" {
		return nil, ErrNoSessionID
	}
	for k := range evs {
		if evs[k].SessionID != sid {
			return nil, errors.New("bus: AppendDurableBatch requires one session per call")
		}
		if evs[k].Type == "" {
			return nil, errors.New("bus: event type required")
		}
		if evs[k].TsUnixNano == 0 {
			evs[k].TsUnixNano = b.cfg.Now().UnixNano()
		}
	}

	lock := b.lockSessionValidated(sid)
	defer lock.Unlock()

	alloc, err := b.cfg.Seqs.For(sid)
	if err != nil {
		b.appendErrors.Add(1)
		return nil, fmt.Errorf("bus: seq allocator: %w", err)
	}
	seqs := make([]uint64, len(evs))
	for k := range evs {
		evs[k].Seq = alloc.Next()
		seqs[k] = evs[k].Seq
	}

	results := b.cfg.Flusher.EnqueueDurableBatch(ctx, evs)

	state := b.stateForLocked(sid)
	var firstErr error
	committed := 0
	for k := range evs {
		if results[k] != nil {
			if firstErr == nil {
				firstErr = results[k]
			}
			b.appendErrors.Add(1)
			if errors.Is(results[k], ErrQueueFull) {
				b.dropped.Add(1)
			}
			continue
		}
		Apply(state, &evs[k])
		b.notify(evs[k])
		b.appendTotal.Add(1)
		committed++
	}
	if committed > 0 {
		b.touchSession(sid)
	}
	if firstErr != nil {
		return seqs, fmt.Errorf("bus: enqueue durable batch: %w", firstErr)
	}
	return seqs, nil
}

func (b *Bus) AppendDurable(ctx context.Context, ev Event) (uint64, error) {
	if b.stopped.Load() {
		return 0, ErrBusStopped
	}
	if !b.started.Load() {
		return 0, ErrBusNotStarted
	}
	if ev.SessionID == "" {
		return 0, ErrNoSessionID
	}
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = b.cfg.Now().UnixNano()
	}
	if ev.Type == "" {
		return 0, errors.New("bus: event type required")
	}

	lock := b.lockSessionValidated(ev.SessionID)
	defer lock.Unlock()

	alloc, err := b.cfg.Seqs.For(ev.SessionID)
	if err != nil {
		b.appendErrors.Add(1)
		return 0, fmt.Errorf("bus: seq allocator: %w", err)
	}
	tentativeSeq := alloc.Current() + 1
	ev.Seq = tentativeSeq

	if err := b.cfg.Flusher.EnqueueDurable(ctx, ev); err != nil {
		b.appendErrors.Add(1)
		if errors.Is(err, ErrQueueFull) {
			b.dropped.Add(1)
		}
		return 0, fmt.Errorf("bus: enqueue durable: %w", err)
	}

	finalSeq := alloc.Next()
	if finalSeq != tentativeSeq {
		return 0, fmt.Errorf("bus: seq race (tentative=%d final=%d)",
			tentativeSeq, finalSeq)
	}

	state := b.stateForLocked(ev.SessionID)
	Apply(state, &ev)
	b.touchSession(ev.SessionID)

	b.notify(ev)
	b.appendTotal.Add(1)
	return ev.Seq, nil
}

func (b *Bus) AppendMany(ctx context.Context, evs []Event) ([]uint64, error) {
	out := make([]uint64, 0, len(evs))
	for i := range evs {
		seq, err := b.Append(ctx, evs[i])
		if err != nil {
			return out, err
		}
		out = append(out, seq)
	}
	return out, nil
}

func (b *Bus) State(sid string) (*SessionState, error) {
	if sid == "" {
		return nil, ErrNoSessionID
	}
	if s, ok := b.states.Load(sid); ok {
		b.touchSession(sid)
		return s.(*SessionState), nil
	}
	res, err := Load(b.cfg.Paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		return nil, err
	}
	actual, loaded := b.states.LoadOrStore(sid, res.State)
	state := actual.(*SessionState)
	if !loaded {
		b.statesCount.Add(1)
	}
	b.touchSession(sid)

	alloc, _ := b.cfg.Seqs.For(sid)
	if alloc != nil {
		alloc.Bump(state.LastSeq)
	}
	return state, nil
}

func (b *Bus) stateForLocked(sid string) *SessionState {
	if s, ok := b.states.Load(sid); ok {
		return s.(*SessionState)
	}
	res, err := Load(b.cfg.Paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		b.log.Warn("bus: cold load failed, starting empty session",
			slog.String("sid", sid), slog.String("err", err.Error()))
		fresh := NewSessionState(sid)
		actual, loaded := b.states.LoadOrStore(sid, fresh)
		if !loaded {
			b.statesCount.Add(1)
		}
		return actual.(*SessionState)
	}
	actual, loaded := b.states.LoadOrStore(sid, res.State)
	if !loaded {
		b.statesCount.Add(1)
	}
	return actual.(*SessionState)
}

func (b *Bus) touchSession(sid string) {
	b.lastTouch.Store(sid, b.cfg.Now().UnixNano())
}

func (b *Bus) sessionLockFor(sid string) *sync.Mutex {
	if v, ok := b.sessionLocks.Load(sid); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := b.sessionLocks.LoadOrStore(sid, mu)
	return actual.(*sync.Mutex)
}

func (b *Bus) lockSessionValidated(sid string) *sync.Mutex {
	for {
		mu := b.sessionLockFor(sid)
		mu.Lock()
		if cur, ok := b.sessionLocks.Load(sid); ok && cur.(*sync.Mutex) == mu {
			return mu
		}
		mu.Unlock()
	}
}

func (b *Bus) LockSession(sid string) func() {
	return b.lockSessionValidated(sid).Unlock
}

func (b *Bus) DropFD(sid string) {
	if b.cfg.Flusher != nil {
		b.cfg.Flusher.DropFD(sid)
	}
}

func (b *Bus) Transcript(sid string) ([]Message, error) {
	return ReadTranscript(b.cfg.Paths, sid)
}

func (b *Bus) FlushPending(ctx context.Context) error {
	if b.cfg.Flusher != nil {
		return b.cfg.Flusher.Flush(ctx)
	}
	return nil
}

func (b *Bus) Compactor(opts CompactorConfig) *Compactor {
	opts.Paths = b.cfg.Paths
	if opts.Seqs == nil {
		opts.Seqs = b.cfg.Seqs
	}
	return NewCompactor(opts)
}

func (b *Bus) Subscribe(sid string, cb func(Event)) (*Subscription, error) {
	if sid == "" {
		return nil, ErrEmptySIDSubscribe
	}
	if cb == nil {
		return nil, ErrNilCallback
	}
	if !b.started.Load() {
		return nil, ErrBusNotStarted
	}
	return b.subscribe(sid, cb), nil
}

func (b *Bus) SubscribeAll(cb func(Event)) (*Subscription, error) {
	if cb == nil {
		return nil, ErrNilCallback
	}
	if !b.started.Load() {
		return nil, ErrBusNotStarted
	}
	return b.subscribe("", cb), nil
}

func (b *Bus) subscribe(sid string, cb func(Event)) *Subscription {
	uq := newUnboundedQueue()
	s := &subscription{
		bus:  b,
		id:   b.nextSubID.Add(1),
		sid:  sid,
		cb:   cb,
		uq:   uq,
		done: make(chan struct{}),
	}

	b.subsMu.Lock()
	if sid == "" {
		b.subsAll = append(b.subsAll, s)
	} else {
		b.subsPerSID[sid] = append(b.subsPerSID[sid], s)
	}
	b.subsMu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		s.run(b.ctx)
	}()

	return &Subscription{bus: b, sub: s}
}

func (s *subscription) run(ctx context.Context) {
	s.uq.run(ctx, s.deliver)
}

func (s *subscription) drainRemaining() {
}

func (s *subscription) deliver(ev Event) {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			s.bus.callbackPanics.Add(1)
			s.bus.log.Error("bus: subscription callback panicked",
				slog.Int64("id", s.id),
				slog.String("sid", s.sid),
				slog.Uint64("seq", ev.Seq),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	s.cb(ev)
}

func (b *Bus) cancelSubscription(s *subscription) {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	b.subsMu.Lock()
	if s.sid == "" {
		b.subsAll = removeSub(b.subsAll, s.id)
	} else {
		if list, ok := b.subsPerSID[s.sid]; ok {
			b.subsPerSID[s.sid] = removeSub(list, s.id)
			if len(b.subsPerSID[s.sid]) == 0 {
				delete(b.subsPerSID, s.sid)
			}
		}
	}
	b.subsMu.Unlock()
	s.uq.stop()
	close(s.done)
}

func removeSub(list []*subscription, id int64) []*subscription {
	for i, s := range list {
		if s.id == id {
			list[i] = list[len(list)-1]
			return list[:len(list)-1]
		}
	}
	return list
}

func (b *Bus) notify(ev Event) {
	b.subsMu.RLock()
	perSID := append([]*subscription(nil), b.subsPerSID[ev.SessionID]...)
	all := append([]*subscription(nil), b.subsAll...)
	b.subsMu.RUnlock()

	b.deliverToSubs(perSID, ev)
	b.deliverToSubs(all, ev)
}

func (b *Bus) deliverToSubs(subs []*subscription, ev Event) {
	for _, s := range subs {
		if s.closed.Load() {
			continue
		}
		s.uq.push(ev)
		b.notifyTotal.Add(1)
	}
}

func (b *Bus) evictionLoop() {
	defer b.wg.Done()

	tick := time.NewTicker(b.cfg.EvictionInterval)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			safego.Run("sessionstore.eviction", b.evictIdleStates)
		case <-b.ctx.Done():
			return
		}
	}
}

func (b *Bus) evictIdleStates() {
	type cand struct {
		sid       string
		lastTouch int64
	}

	cutoff := b.cfg.Now().UnixNano() - b.cfg.StateIdleEvictAfter.Nanoseconds()

	var all []cand
	b.lastTouch.Range(func(k, v any) bool {
		all = append(all, cand{sid: k.(string), lastTouch: v.(int64)})
		return true
	})

	toEvict := make(map[string]bool)
	for _, c := range all {
		if c.lastTouch < cutoff {
			toEvict[c.sid] = true
		}
	}

	statesCount := int(b.statesCount.Load())
	if statesCount > b.cfg.MaxStatesInMemory {
		overage := statesCount - b.cfg.MaxStatesInMemory
		sort.Slice(all, func(i, j int) bool { return all[i].lastTouch < all[j].lastTouch })
		added := 0
		for _, c := range all {
			if added >= overage {
				break
			}
			if !toEvict[c.sid] {
				toEvict[c.sid] = true
				added++
			}
		}
	}

	for sid := range toEvict {
		b.evictLocked(sid, true)
	}
}

func (b *Bus) evictLocked(sid string, persist bool) {
	mu := b.lockSessionValidated(sid)
	defer mu.Unlock()
	if persist {
		_ = b.SyncMetaToDisk(sid)
		b.cfg.Seqs.Drop(sid)
	}
	b.sessionLocks.Delete(sid)
	if _, ok := b.states.LoadAndDelete(sid); ok {
		b.statesCount.Add(-1)
		b.statesEvicted.Add(1)
	}
	b.lastTouch.Delete(sid)
}

func (b *Bus) SyncMetaToDisk(sid string) error {
	if sid == "" {
		return ErrNoSessionID
	}
	s, ok := b.states.Load(sid)
	if !ok {
		return nil
	}
	state := s.(*SessionState)
	state.RLock()
	meta := &Meta{
		SessionID:     sid,
		AppID:         state.AppID,
		UserID:        state.UserID,
		FirstSeq:      state.FirstSeq,
		LastSeq:       state.LastSeq,
		EventCount:    state.EventCount,
		StartedAtNano: state.StartedAtNano,
		UpdatedAtNano: state.LastEventTsNano,
		Title:         state.Title,
		Workspace:     state.Workspace,
		Workdir:       state.Workdir,
		Partial:       state.Partial,
		Preview:       previewFromMessages(state.Messages),
	}
	state.RUnlock()
	dir := b.cfg.Paths.SessionDir(sid)
	return WriteMetaAtomic(dir, meta, false)
}

func (b *Bus) Drop(sid string) {
	b.evictLocked(sid, false)
	if b.cfg.Flusher != nil {
		b.cfg.Flusher.DropFD(sid)
	}
	if b.cfg.Seqs != nil {
		b.cfg.Seqs.Drop(sid)
	}
}

type BusStats struct {
	AppendTotal     uint64
	AppendErrors    uint64
	Dropped         uint64
	NotifyTotal     uint64
	CallbackPanics  uint64
	SubscriberDrops uint64
	SubscriberKicks uint64
	StatesLoaded    int
	StatesEvicted   uint64
	Subscriptions   int
}

func (b *Bus) Stats() BusStats {
	statesCount := int(b.statesCount.Load())

	b.subsMu.RLock()
	subs := len(b.subsAll)
	for _, list := range b.subsPerSID {
		subs += len(list)
	}
	b.subsMu.RUnlock()

	return BusStats{
		AppendTotal:     b.appendTotal.Load(),
		AppendErrors:    b.appendErrors.Load(),
		Dropped:         b.dropped.Load(),
		NotifyTotal:     b.notifyTotal.Load(),
		CallbackPanics:  b.callbackPanics.Load(),
		SubscriberDrops: b.subscriberDrops.Load(),
		SubscriberKicks: b.subscriberKicks.Load(),
		StatesLoaded:    statesCount,
		StatesEvicted:   b.statesEvicted.Load(),
		Subscriptions:   subs,
	}
}
