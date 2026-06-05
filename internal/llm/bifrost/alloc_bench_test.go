package bifrost

import (
	"testing"
)

// sink forces escape — assigning to an interface{} variable that the
// compiler can't see through is the exact pattern Bifrost's ctx.Value
// triggers in the real call path. Without it, escape analysis would
// stack-allocate the test struct and hide the pool's real win.
var sink any

// BenchmarkRouteInfoPool measures the per-call alloc cost of the pooled
// routeInfo on the Chat/Embed hot path. Pre-Phase-4 each call allocated
// a fresh *routeInfo on the heap. With the pool, the steady-state cost
// drops to ~0 allocs (Get returns a hot pointer).
func BenchmarkRouteInfoPool(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local any
		for pb.Next() {
			r := acquireRouteInfo(true, "sk-test", "", "")
			local = r // force escape, mimics ctx.Value(interface{})
			releaseRouteInfo(r)
		}
		sink = local
	})
}

// BenchmarkRouteInfoFreshAlloc is the pre-pool baseline — a fresh heap
// alloc per call. We keep it to track the win in benchstat.
func BenchmarkRouteInfoFreshAlloc(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local any
		for pb.Next() {
			r := &routeInfo{BYOK: true, APIKey: "sk-test"}
			local = r // force escape
		}
		sink = local
	})
}
