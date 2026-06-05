package sessionstore

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBus_Subscribe_RejectsEmptySID(t *testing.T) {
	bus, _, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	if _, err := bus.Subscribe("", func(Event) {}); err == nil {
		t.Fatal("expected error when subscribing with empty sid")
	}
}

func TestBus_Subscribe_RejectsNilCallback(t *testing.T) {
	bus, _, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	if _, err := bus.Subscribe("sess", nil); err == nil {
		t.Fatal("expected error with nil cb")
	}
	if _, err := bus.SubscribeAll(nil); err == nil {
		t.Fatal("expected error with nil cb on SubscribeAll")
	}
}

func TestBus_NoSubscriberLeak_AcrossSessions(t *testing.T) {
	bus, _, cleanup := startBus(t, t.TempDir())
	defer cleanup()

	var aSeen, bSeen atomic.Int64
	var doneA, doneB sync.WaitGroup
	doneA.Add(10)
	doneB.Add(10)

	subA, err := bus.Subscribe("sess-A", func(ev Event) {
		if ev.SessionID != "sess-A" {
			t.Errorf("subA leak: got event for %s", ev.SessionID)
		}
		aSeen.Add(1)
		doneA.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subA.Cancel()

	subB, err := bus.Subscribe("sess-B", func(ev Event) {
		if ev.SessionID != "sess-B" {
			t.Errorf("subB leak: got event for %s", ev.SessionID)
		}
		bSeen.Add(1)
		doneB.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subB.Cancel()

	for i := 0; i < 10; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg("sess-A", "a")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg("sess-B", "b")); err != nil {
			t.Fatal(err)
		}
	}
	doneA.Wait()
	doneB.Wait()

	if aSeen.Load() != 10 || bSeen.Load() != 10 {
		t.Fatalf("counts: A=%d B=%d", aSeen.Load(), bSeen.Load())
	}
}

func TestBus_SubscribeAll_OnlySeesAfterSubscribe(t *testing.T) {
	bus, _, cleanup := startBus(t, t.TempDir())
	defer cleanup()

	var count atomic.Int64
	var wg sync.WaitGroup
	wg.Add(5)
	sub, err := bus.SubscribeAll(func(ev Event) {
		count.Add(1)
		wg.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	for i := 0; i < 5; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg("any", "x")); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if c := count.Load(); c != 5 {
		t.Fatalf("global got %d want 5", c)
	}
}

func TestBus_SlowSubscriber_DoesNotBlockOthers(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 65536,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	flusher.Start()
	bus, _ := NewBus(BusConfig{
		Paths:                  paths,
		Flusher:                flusher,
		SubscriberQueueSize:    200,
		SubscriberMaxSlowDrops: 1_000_000,
	})
	bus.Start(context.Background())

	sid := "sess-mix"
	slowReleased := make(chan struct{})
	defer func() {
		select {
		case <-slowReleased:
		default:
			close(slowReleased)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}()
	var slowReceived, fastReceived atomic.Int64
	var fastWG sync.WaitGroup
	fastWG.Add(100)

	subSlow, err := bus.Subscribe(sid, func(ev Event) {
		slowReceived.Add(1)
		select {
		case <-slowReleased:
		case <-time.After(3 * time.Second):
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subSlow.Cancel()

	subFast, err := bus.Subscribe(sid, func(ev Event) {
		fastReceived.Add(1)
		fastWG.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subFast.Cancel()

	start := time.Now()
	for i := 0; i < 100; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg(sid, "x")); err != nil {
			t.Fatal(err)
		}
	}
	appendDur := time.Since(start)
	if appendDur > 500*time.Millisecond {
		t.Errorf("Append took %v with blocked slow subscriber — should be <500ms (blocking!)", appendDur)
	}

	fastWG.Wait()
	fastDur := time.Since(start)
	if fastReceived.Load() != 100 {
		t.Fatalf("fast subscriber received %d, expected 100", fastReceived.Load())
	}
	if fastDur > 1*time.Second {
		t.Errorf("fast subscriber delivery took %v with slow blocked — too slow (isolation broken?)", fastDur)
	}
	close(slowReleased)
}

// syncBuffer is a concurrency-safe io.Writer over a bytes.Buffer — the
// bus logs subscriber-panic recoveries from multiple goroutines, so a
// raw bytes.Buffer sink would race the test's String() read.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestBus_PanicInSubscriber_LoggedAndIsolated(t *testing.T) {
	logBuf := &syncBuffer{}
	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 1, QueueCapPerShard: 1024,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	flusher.Start()
	bus, _ := NewBus(BusConfig{
		Paths:   paths,
		Flusher: flusher,
		Logger:  slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	bus.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}()

	sid := "sess-panic"
	var panicCount atomic.Int64
	var goodReceived atomic.Int64
	var wg, panicWG sync.WaitGroup
	wg.Add(5)
	panicWG.Add(5)

	subPanic, err := bus.Subscribe(sid, func(ev Event) {
		panicCount.Add(1)
		panicWG.Done()
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subPanic.Cancel()

	subGood, err := bus.Subscribe(sid, func(ev Event) {
		goodReceived.Add(1)
		wg.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subGood.Cancel()

	for i := 0; i < 5; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg(sid, "x")); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	panicWG.Wait()

	if panicCount.Load() == 0 {
		t.Fatal("expected panic subscriber to have been called")
	}
	if goodReceived.Load() != 5 {
		t.Fatalf("good subscriber received %d, expected 5 (panic broke isolation!)", goodReceived.Load())
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "panic") {
		t.Fatalf("expected panic to be LOGGED (not silent); got log: %q", logs)
	}
	stats := bus.Stats()
	if stats.CallbackPanics == 0 {
		t.Fatal("CallbackPanics metric must increment")
	}
}

func TestBus_NoGoroutineLeak_AfterStop(t *testing.T) {
	runtime.GC()
	baseline := runtime.NumGoroutine()

	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 4, QueueCapPerShard: 4096,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	flusher.Start()
	bus, _ := NewBus(BusConfig{Paths: paths, Flusher: flusher})
	bus.Start(context.Background())

	for i := 0; i < 20; i++ {
		sid := fmt.Sprintf("s%d", i)
		sub, _ := bus.Subscribe(sid, func(Event) {})
		_ = sub
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bus.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := flusher.Stop(ctx); err != nil {
		t.Fatalf("flusher stop: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	final := runtime.NumGoroutine()
	if final > baseline+3 {
		t.Fatalf("goroutine leak: baseline=%d final=%d", baseline, final)
	}
}

func TestBus_StateEviction_FreesIdleSessions(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 1024,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	flusher.Start()

	// frozenNs is read by the bus's eviction goroutine (via Now()) AND
	// advanced by the test — atomic to avoid a data race on the clock.
	var frozenNs atomic.Int64
	frozenNs.Store(time.Now().UnixNano())
	bus, _ := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		StateIdleEvictAfter: 10 * time.Millisecond,
		EvictionInterval:    20 * time.Millisecond,
		Now:                 func() time.Time { return time.Unix(0, frozenNs.Load()) },
	})
	bus.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}()

	for i := 0; i < 10; i++ {
		sid := fmt.Sprintf("idle-%d", i)
		if _, err := bus.Append(context.Background(), makeUserMsg(sid, "x")); err != nil {
			t.Fatal(err)
		}
	}
	if stats := bus.Stats(); stats.StatesLoaded != 10 {
		t.Fatalf("expected 10 states loaded, got %d", stats.StatesLoaded)
	}

	frozenNs.Store(frozenNs.Load() + int64(time.Hour))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Stats().StatesEvicted >= 10 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stats := bus.Stats()
	if stats.StatesEvicted < 10 {
		t.Fatalf("expected 10 evictions, got %d (loaded=%d)", stats.StatesEvicted, stats.StatesLoaded)
	}
}

func TestBus_StateEviction_OverCapEvictsOldest(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 1024,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	flusher.Start()

	clock := atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	tick := func() time.Time {
		v := clock.Add(int64(10 * time.Millisecond))
		return time.Unix(0, v)
	}

	bus, _ := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		MaxStatesInMemory:   3,
		StateIdleEvictAfter: 1 * time.Hour,
		EvictionInterval:    20 * time.Millisecond,
		Now:                 tick,
	})
	bus.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}()

	for i := 0; i < 6; i++ {
		sid := fmt.Sprintf("cap-%d", i)
		if _, err := bus.Append(context.Background(), makeUserMsg(sid, "x")); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Stats().StatesLoaded <= 3 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	stats := bus.Stats()
	if stats.StatesLoaded > 3 {
		t.Fatalf("expected at most 3 states, got %d (evicted=%d)", stats.StatesLoaded, stats.StatesEvicted)
	}
	if stats.StatesEvicted < 3 {
		t.Fatalf("expected at least 3 evictions, got %d", stats.StatesEvicted)
	}
}

func TestBus_CancelledSubscription_StopsReceivingEvents(t *testing.T) {
	bus, _, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	sid := "sess-cancel"

	var received atomic.Int64
	sub, err := bus.Subscribe(sid, func(Event) {
		received.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		bus.Append(context.Background(), makeUserMsg(sid, "x"))
	}
	time.Sleep(50 * time.Millisecond)
	if r := received.Load(); r != 5 {
		t.Fatalf("phase1 received %d, want 5", r)
	}

	sub.Cancel()

	for i := 0; i < 5; i++ {
		bus.Append(context.Background(), makeUserMsg(sid, "x"))
	}
	time.Sleep(50 * time.Millisecond)
	if r := received.Load(); r != 5 {
		t.Fatalf("phase2 received %d after cancel, want still 5", r)
	}
}

func TestBus_Append_RejectsWhenStopped(t *testing.T) {
	bus, flusher, _ := startBus(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bus.Stop(ctx)
	defer flusher.Stop(ctx)

	if _, err := bus.Append(context.Background(), makeUserMsg("s", "x")); err == nil {
		t.Fatal("expected ErrBusStopped after Stop")
	}
}

func TestBus_Append_RejectsWhenNotStarted(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{Paths: paths, NumShards: 1})
	flusher.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		flusher.Stop(ctx)
	}()
	bus, _ := NewBus(BusConfig{Paths: paths, Flusher: flusher})
	if _, err := bus.Append(context.Background(), makeUserMsg("s", "x")); err == nil {
		t.Fatal("expected ErrBusNotStarted before Start")
	}
}

func TestBus_StateAccess_TouchesLRU(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, _ := NewDiskFlusher(DiskFlusherConfig{Paths: paths, NumShards: 1, QueueCapPerShard: 1024, BatchMax: 100, FlushInterval: 2 * time.Millisecond})
	flusher.Start()

	clock := atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	tick := func() time.Time {
		v := clock.Add(int64(time.Millisecond))
		return time.Unix(0, v)
	}

	bus, _ := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		MaxStatesInMemory:   2,
		StateIdleEvictAfter: 1 * time.Hour,
		EvictionInterval:    50 * time.Millisecond,
		Now:                 tick,
	})
	bus.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}()

	bus.Append(context.Background(), makeUserMsg("old-1", "x"))
	bus.Append(context.Background(), makeUserMsg("old-2", "x"))
	bus.Append(context.Background(), makeUserMsg("new-3", "x"))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Stats().StatesLoaded <= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if bus.Stats().StatesLoaded > 2 {
		t.Fatalf("expected eviction down to 2, got %d", bus.Stats().StatesLoaded)
	}
}
