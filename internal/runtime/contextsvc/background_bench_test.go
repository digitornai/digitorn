package contextsvc

import (
	"context"
	"strconv"
	"testing"
)

func BenchmarkBackground_TouchParallel(b *testing.B) {
	b.ReportAllocs()
	bg := NewBackground(noopCounter{}, fakeView{}, func(Result) {})
	bg.Start(8)
	defer bg.Stop()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			bg.Touch("sess-" + strconv.Itoa(i&8191))
			i++
		}
	})
}

type noopCounter struct{}

func (noopCounter) CountTotal(_ context.Context, _ []string, _, _ string) (int, error) {
	return 1, nil
}
