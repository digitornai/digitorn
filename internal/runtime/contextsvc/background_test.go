package contextsvc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCounter struct {
	delay time.Duration
	calls atomic.Int64
}

func (f *fakeCounter) CountTotal(ctx context.Context, texts []string, _, _ string) (int, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return len(texts), nil
}

type fakeView struct{}

func (fakeView) ContextView(string) (View, bool) {
	return View{
		System:   []string{"system prompt"},
		Tools:    []string{"tool a", "tool b"},
		Messages: []string{"m1", "m2", "m3"},
		Provider: "openai", Model: "gpt-4o",
	}, true
}

func TestBackground_ComputesBreakdownAndNotifies(t *testing.T) {
	got := make(chan Result, 1)
	b := NewBackground(&fakeCounter{}, fakeView{}, func(r Result) { got <- r })
	b.Start(2)
	defer b.Stop()

	b.Touch("sess-1")
	select {
	case r := <-got:
		if r.System != 1 || r.Tools != 2 || r.Messages != 3 || r.Total != 6 {
			t.Fatalf("breakdown = sys:%d tools:%d msgs:%d total:%d, want 1/2/3/6", r.System, r.Tools, r.Messages, r.Total)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no recompute delivered within 2s")
	}
}

func TestBackground_TouchNeverBlocks(t *testing.T) {
	b := NewBackground(&fakeCounter{delay: 3 * time.Second}, fakeView{}, func(Result) {})
	b.Start(1)
	defer b.Stop()

	start := time.Now()
	for i := 0; i < 1000; i++ {
		b.Touch("sess-x")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Touch blocked: 1000 calls took %v (must be ~instant)", elapsed)
	}
}

func TestBackground_CoalescesPerSession(t *testing.T) {
	fc := &fakeCounter{delay: 20 * time.Millisecond}
	var mu sync.Mutex
	results := 0
	b := NewBackground(fc, fakeView{}, func(Result) { mu.Lock(); results++; mu.Unlock() })
	b.Start(1)
	defer b.Stop()

	for i := 0; i < 500; i++ {
		b.Touch("sess-burst")
	}
	time.Sleep(300 * time.Millisecond)
	if c := fc.calls.Load(); c >= 500 {
		t.Fatalf("no coalescing: %d counts for 500 touches", c)
	}
}

func TestBackground_WorkerErrorNoEstimate(t *testing.T) {
	called := atomic.Bool{}
	b := NewBackground(errCounter{}, fakeView{}, func(Result) { called.Store(true) })
	b.Start(1)
	defer b.Stop()
	b.Touch("sess-err")
	time.Sleep(200 * time.Millisecond)
	if called.Load() {
		t.Fatal("onResult must NOT fire on a counter error — no estimate fallback")
	}
}

type errCounter struct{}

func (errCounter) CountTotal(context.Context, []string, string, string) (int, error) {
	return 0, context.DeadlineExceeded
}
