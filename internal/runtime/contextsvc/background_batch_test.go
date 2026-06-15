package contextsvc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBatchCounter implements BOTH CountTotal and CountEach, counting calls so a
// test can assert which path the recount took. CountEach returns 1 token per
// text → each bucket's count equals its text count (matching fakeView's 1/2/3).
type fakeBatchCounter struct {
	eachCalls  atomic.Int64
	totalCalls atomic.Int64
}

func (f *fakeBatchCounter) CountTotal(_ context.Context, texts []string, _, _ string) (int, error) {
	f.totalCalls.Add(1)
	return len(texts), nil
}

func (f *fakeBatchCounter) CountEach(_ context.Context, texts []string, _, _ string) ([]int, error) {
	f.eachCalls.Add(1)
	out := make([]int, len(texts))
	for i := range out {
		out[i] = 1
	}
	return out, nil
}

// TestBackground_BatchedRecount: a counter that supports CountEach is counted in
// ONE batched RPC pass — same breakdown as the per-bucket fallback, but with no
// per-bucket CountTotal calls.
func TestBackground_BatchedRecount(t *testing.T) {
	got := make(chan Result, 1)
	fc := &fakeBatchCounter{}
	b := NewBackground(fc, fakeView{}, func(r Result) { got <- r })
	b.Start(1)
	defer b.Stop()

	b.Touch("s")
	select {
	case r := <-got:
		if r.System != 1 || r.Tools != 2 || r.Messages != 3 || r.Total != 6 {
			t.Fatalf("batched breakdown = %d/%d/%d/%d, want 1/2/3/6", r.System, r.Tools, r.Messages, r.Total)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no recompute delivered within 2s")
	}
	if e := fc.eachCalls.Load(); e != 1 {
		t.Errorf("CountEach calls = %d, want 1 (one batched pass)", e)
	}
	if tc := fc.totalCalls.Load(); tc != 0 {
		t.Errorf("CountTotal calls = %d, want 0 (batched path must not fall back)", tc)
	}
}
