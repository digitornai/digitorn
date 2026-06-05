package contextsvc

import (
	"context"
	"strconv"
	"testing"
)

// BenchmarkBackground_TouchParallel measures the per-event signal throughput :
// how fast the runtime can Touch the service from many goroutines at once. This
// is the hot-path cost (one per message/tool/turn). Distinct session ids stress
// the dedup map + queue. The recompute itself is a no-op counter so we isolate
// the Touch path.
func BenchmarkBackground_TouchParallel(b *testing.B) {
	b.ReportAllocs()
	bg := NewBackground(noopCounter{}, fakeView{}, func(Result) {})
	bg.Start(8)
	defer bg.Stop()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// A spread of session ids so the dedup map is exercised, not a single
			// hot key (which would coalesce to almost nothing).
			bg.Touch("sess-" + strconv.Itoa(i&8191))
			i++
		}
	})
}

type noopCounter struct{}

func (noopCounter) CountTotal(_ context.Context, _ []string, _, _ string) (int, error) {
	return 1, nil
}
