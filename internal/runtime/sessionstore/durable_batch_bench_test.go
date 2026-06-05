package sessionstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// setupDurableBus builds a Bus whose flusher fsyncs every batch, so
// AppendDurable pays the real on-disk durability cost. Baseline for the
// group-commit optimization (Finding #1).
func setupDurableBus(b *testing.B) (*Bus, func()) {
	b.Helper()
	paths := NewPaths(b.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        8,
		QueueCapPerShard: 8192,
		BatchMax:         500,
		FlushInterval:    2 * time.Millisecond,
		FDCachePerShard:  64,
		PerSidQuotaPct:   80,
		Fsync:            true,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		b.Fatal(err)
	}
	bus, err := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		EvictionInterval:    1 * time.Hour,
		StateIdleEvictAfter: 1 * time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	return bus, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}
}

func toolResultEvent(i int) Event {
	return Event{
		Type: EventToolResult,
		Tool: &ToolPayload{CallID: fmt.Sprintf("call-%d", i), Status: "completed", Output: "ok"},
	}
}

// BenchmarkPersistToolResults_SerialDurable persists N tool results into
// ONE session via sequential AppendDurable — the exact shape of
// engine.persistToolResults. Each durable append blocks on its own fsync,
// so wall-clock scales ~linearly with N. Group-commit (Finding #1) must
// collapse this toward constant in N.
func BenchmarkPersistToolResults_SerialDurable(b *testing.B) {
	for _, n := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("tools=%d", n), func(b *testing.B) {
			bus, cleanup := setupDurableBus(b)
			defer cleanup()
			ctx := context.Background()
			sid := "persist-bench"
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for k := 0; k < n; k++ {
					ev := toolResultEvent(k)
					ev.SessionID = sid
					if _, err := bus.AppendDurable(ctx, ev); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkPersistToolResults_BatchDurable is the group-commit form: the N
// tool results of one round persist in a single AppendDurableBatch, riding
// one fsync. Compare ns/op against SerialDurable at the same tool count.
func BenchmarkPersistToolResults_BatchDurable(b *testing.B) {
	for _, n := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("tools=%d", n), func(b *testing.B) {
			bus, cleanup := setupDurableBus(b)
			defer cleanup()
			ctx := context.Background()
			sid := "persist-bench"
			evs := make([]Event, n)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for k := 0; k < n; k++ {
					evs[k] = toolResultEvent(k)
					evs[k].SessionID = sid
				}
				if _, err := bus.AppendDurableBatch(ctx, evs); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
