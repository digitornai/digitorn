package runtime_test

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	runtimepkg "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/turn"
)

// CONCURRENT SESSIONS — runtime ceiling measurement.
//
// Methodology :
//   - N goroutines, each = 1 session × 1 turn.
//   - LLM stub returns instantly. The bench measures PURE runtime
//     overhead — no network, no provider latency. Real-world throughput
//     is min(this ceiling, LLM rate-limit × worker count).
//   - SessionState is per-goroutine to mimic "Bus.State(sid) returns a
//     fresh per-sid state" — no cross-session contention.
//   - We measure : wall-clock for the whole fan-out, throughput, p50 / p99
//     latency per individual turn, error count.
//
// To run :
//   go test -bench BenchmarkConcurrency_RuntimeOnly -benchtime=1x -run=^$ ./internal/runtime
//
// Results are printed in a table at the bottom of the bench output.

func buildBenchApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "bench-app"},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID:           "primary",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "you are helpful",
			}},
		},
		BundleDir: "/tmp/bench-app",
	}
}

func newBenchEngine(apps runtimepkg.AppLookup, sess runtimepkg.SessionAccess, lc runtimepkg.LLMChat, pool *turn.Pool) *runtimepkg.Engine {
	return &runtimepkg.Engine{
		Apps:     apps,
		Sessions: sess,
		LLM:      lc,
		Pool:     pool,
		IDGen:    benchIDGen(),
		Logger:   slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1})),
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// benchIDGen produces non-colliding IDs without uuid's RNG overhead.
func benchIDGen() turn.IDGen {
	var n uint64
	return func() string {
		v := atomic.AddUint64(&n, 1)
		return fmt.Sprintf("turn-%d", v)
	}
}

// benchPool returns an unbounded Pool (all tiers uncapped) so turn.New never
// fails on a missing Pool. A benchmark that constructs &Engine{} directly MUST
// wire this and IDGen, else turn.New errors and — if the Run error is ignored —
// the bench silently times only the aborted-turn path, undercounting the real
// hot path by ~10x.
func benchPool() *turn.Pool { return turn.NewPool(turn.PoolConfig{}) }

type concResult struct {
	N          int
	Wall       time.Duration
	Throughput float64 // turns/sec
	P50, P99   time.Duration
	Errors     int
	HeapMB     uint64
	Goroutines int
}

func runConcurrent(b *testing.B, n int, poolCap int) concResult {
	b.Helper()
	app := buildBenchApp()
	apps := &stubApps{app: app}
	llmStub := &stubLLM{resp: &llm.ChatResponse{Content: "ok", Model: "x"}}
	pool := turn.NewPool(turn.PoolConfig{GlobalCap: poolCap, PerAppCap: poolCap, PerUserCap: poolCap})

	latencies := make([]time.Duration, n)
	var errCount atomic.Int32

	// Pre-allocate per-goroutine sessions so allocation isn't counted.
	sessions := make([]*stubSessions, n)
	for i := 0; i < n; i++ {
		sessions[i] = &stubSessions{
			state:     sessionstore.NewSessionState(fmt.Sprintf("s-%d", i)),
			appendSeq: 1,
		}
	}

	// Force a GC + warm up Go runtime before starting the clock.
	debug.FreeOSMemory()
	var mem0 runtime.MemStats
	runtime.ReadMemStats(&mem0)

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ge := newBenchEngine(apps, sessions[i], llmStub, pool)
			t0 := time.Now()
			_, err := ge.Run(context.Background(), runtimepkg.TurnInput{
				AppID:     "bench-app",
				SessionID: fmt.Sprintf("s-%d", i),
				UserID:    fmt.Sprintf("u-%d", i%100), // 100 distinct users
			})
			latencies[i] = time.Since(t0)
			if err != nil {
				errCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	var mem1 runtime.MemStats
	runtime.ReadMemStats(&mem1)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)/2]
	p99 := latencies[(len(latencies)*99)/100]

	return concResult{
		N:          n,
		Wall:       wall,
		Throughput: float64(n) / wall.Seconds(),
		P50:        p50,
		P99:        p99,
		Errors:     int(errCount.Load()),
		HeapMB:     (mem1.HeapAlloc - mem0.HeapAlloc) / 1024 / 1024,
		Goroutines: runtime.NumGoroutine(),
	}
}

// slowLLM simulates a realistic LLM call latency by sleeping. Used by
// the realistic concurrency bench to measure how many sessions can be
// HELD IN FLIGHT simultaneously when each turn is actually waiting on
// a provider response.
type slowLLM struct {
	delay time.Duration
}

func (s *slowLLM) Chat(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.ChatResponse{Content: "ok", Model: "x"}, nil
}

func runConcurrentRealistic(b *testing.B, n int, poolCap int, llmDelay time.Duration) concResult {
	b.Helper()
	app := buildBenchApp()
	apps := &stubApps{app: app}
	llmStub := &slowLLM{delay: llmDelay}
	pool := turn.NewPool(turn.PoolConfig{GlobalCap: poolCap, PerAppCap: poolCap, PerUserCap: poolCap})

	latencies := make([]time.Duration, n)
	var errCount atomic.Int32

	sessions := make([]*stubSessions, n)
	for i := 0; i < n; i++ {
		sessions[i] = &stubSessions{
			state:     sessionstore.NewSessionState(fmt.Sprintf("s-%d", i)),
			appendSeq: 1,
		}
	}

	debug.FreeOSMemory()
	var mem0 runtime.MemStats
	runtime.ReadMemStats(&mem0)

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ge := newBenchEngine(apps, sessions[i], llmStub, pool)
			t0 := time.Now()
			_, err := ge.Run(context.Background(), runtimepkg.TurnInput{
				AppID:     "bench-app",
				SessionID: fmt.Sprintf("s-%d", i),
				UserID:    fmt.Sprintf("u-%d", i%100),
			})
			latencies[i] = time.Since(t0)
			if err != nil {
				errCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	var mem1 runtime.MemStats
	runtime.ReadMemStats(&mem1)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return concResult{
		N:          n,
		Wall:       wall,
		Throughput: float64(n) / wall.Seconds(),
		P50:        latencies[len(latencies)/2],
		P99:        latencies[(len(latencies)*99)/100],
		Errors:     int(errCount.Load()),
		HeapMB:     (mem1.HeapAlloc - mem0.HeapAlloc) / 1024 / 1024,
		Goroutines: runtime.NumGoroutine(),
	}
}

// BenchmarkConcurrency_Realistic_100msLLM simulates real-world :
// every turn waits 100ms (typical LLM streaming first-token latency)
// while N other turns run concurrently. This tells us how many
// SIMULTANEOUS in-flight sessions the runtime can hold while the LLM
// is busy — the actual "concurrent users on a single daemon"
// capacity assuming the LLM provider rate limit is not the bottleneck.
func BenchmarkConcurrency_Realistic_100msLLM(b *testing.B) {
	b.StopTimer()
	scales := []struct {
		n       int
		poolCap int
	}{
		{100, 16384},
		{1000, 16384},
		{10000, 16384},
		{50000, 65536},
	}
	results := make([]concResult, 0, len(scales))
	for _, s := range scales {
		r := runConcurrentRealistic(b, s.n, s.poolCap, 100*time.Millisecond)
		results = append(results, r)
	}
	b.StartTimer()

	fmt.Println()
	fmt.Println("=== CONCURRENT IN-FLIGHT SESSIONS — 100ms LLM latency ===")
	fmt.Printf("%-8s %-10s %-14s %-12s %-12s %-8s %-10s\n",
		"N", "wall", "throughput", "p50", "p99", "errors", "heapMB")
	for _, r := range results {
		fmt.Printf("%-8d %-10v %-14s %-12v %-12v %-8d %-10d\n",
			r.N, r.Wall.Round(time.Millisecond),
			fmt.Sprintf("%.0f t/s", r.Throughput),
			r.P50.Round(time.Millisecond),
			r.P99.Round(time.Millisecond),
			r.Errors,
			r.HeapMB,
		)
	}
	fmt.Println("=========================================================")
	fmt.Println("Interpretation : with a 100ms LLM round-trip, each user")
	fmt.Println("holds a slot for ~100ms. Throughput ceiling = poolCap × 10/s.")
	fmt.Println("e.g. poolCap=10000 → up to ~100k turns/s sustained.")
	fmt.Println()
}

// BenchmarkConcurrency_RuntimeOnly answers the "how many concurrent
// runtime sessions can we drive" question. Output is a clean table on
// stdout — read the row at your target N.
func BenchmarkConcurrency_RuntimeOnly(b *testing.B) {
	b.StopTimer()
	scales := []struct {
		n       int
		poolCap int
	}{
		{10, 16384},
		{100, 16384},
		{1000, 16384},
		{10000, 16384},
		{50000, 65536},
		{100000, 131072},
	}
	results := make([]concResult, 0, len(scales))
	for _, s := range scales {
		r := runConcurrent(b, s.n, s.poolCap)
		results = append(results, r)
	}
	b.StartTimer()

	fmt.Println()
	fmt.Println("=== CONCURRENT SESSIONS — runtime ceiling (no-op LLM) ===")
	fmt.Printf("%-8s %-10s %-14s %-10s %-10s %-8s %-10s\n",
		"N", "wall", "throughput", "p50", "p99", "errors", "heapMB")
	for _, r := range results {
		fmt.Printf("%-8d %-10v %-14s %-10v %-10v %-8d %-10d\n",
			r.N, r.Wall.Round(time.Millisecond),
			fmt.Sprintf("%.0f t/s", r.Throughput),
			r.P50.Round(time.Microsecond),
			r.P99.Round(time.Microsecond),
			r.Errors,
			r.HeapMB,
		)
	}
	fmt.Println("=========================================================")
	fmt.Println()
}
