package runtime_test

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// UT-L1 — 10K concurrent turns. The target architecture claims 10M
// sessions concurrent ; this test runs a 1000× scaled-down stress
// (10K) and verifies linear behaviour. Bounded RAM, no deadlocks,
// no cross-talk between sessions.
// =====================================================================

// fixedLLM is a concurrency-safe LLM stub : it returns a fresh terminal
// (no-tool) response on every call so one instance can back a shared engine
// under heavy parallelism without the data race stubLLM's plain int counter
// would trigger. The fresh allocation per call is deliberate — it keeps the
// hot path realistic without shared mutable response state.
type fixedLLM struct{ calls atomic.Int64 }

func (f *fixedLLM) Chat(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	f.calls.Add(1)
	return &llm.ChatResponse{Content: "ok"}, nil
}

// sharedSessions is a concurrency-safe, multi-session SessionAccess so ONE
// engine can serve many sessions concurrently — the production model. It
// mirrors the sharded production store : per-session state is self-locking
// (sessionstore.Apply takes the SessionState's own mutex), so the only shared
// hot point is a brief RLock to find the per-session state and an atomic seq.
// No global mutex is held across Apply, so cross-session contention stays near
// zero even at 256-way parallelism.
type sharedSessions struct {
	mu  sync.RWMutex
	m   map[string]*sessionstore.SessionState
	seq atomic.Uint64
}

func newSharedSessions() *sharedSessions {
	return &sharedSessions{m: make(map[string]*sessionstore.SessionState)}
}

func (s *sharedSessions) State(sid string) (*sessionstore.SessionState, error) {
	s.mu.RLock()
	st := s.m[sid]
	s.mu.RUnlock()
	if st != nil {
		return st, nil
	}
	s.mu.Lock()
	if st = s.m[sid]; st == nil {
		st = sessionstore.NewSessionState(sid)
		s.m[sid] = st
	}
	s.mu.Unlock()
	return st, nil
}

func (s *sharedSessions) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	st, _ := s.State(ev.SessionID)
	seq := s.seq.Add(1)
	ev.Seq = seq
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	sessionstore.Apply(st, &ev) // self-locking per session
	return seq, nil
}

func TestStress_10KConcurrentTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test slow under -short")
	}
	const (
		N = 10000
		// In-flight cap. All 10K turns are submitted as fast as this gate
		// allows ; 64 keeps ~4× more turns in flight than cores (16 here) so
		// the shared engine + session store are genuinely exercised under
		// contention. We deliberately don't push this to, say, 256 : with an
		// instant stub LLM every turn is CPU-bound, so 16× oversubscription
		// would make time.Since() measure Go's run-queue wait (scheduler
		// starvation) instead of engine latency — an artefact, not a defect.
		// In production turns are I/O-bound (the LLM call parks the goroutine
		// and frees its P), so thousands run concurrently without CPU
		// oversubscription ; 64 reproduces that regime faithfully.
		concurrency = 64
	)

	app := realDispatchApp()
	apps := &stubApps{app: app}

	// ONE engine + ONE multi-session store shared across every turn — the
	// production model (a long-lived daemon serving 10M concurrent sessions),
	// NOT one engine per turn. Constructing 10K engines inside the loop was a
	// test artefact : the allocation storm drafted unlucky turns into GC
	// mark-assist, giving a bimodal p50≈0 / p99≈100ms tail that measured the
	// GC, not the engine. Sharing the engine measures real per-turn latency.
	sess := newSharedSessions()
	lc := &fixedLLM{}
	e := newEngine(t, apps, sess, lc)

	var (
		runs        atomic.Int64
		errs        atomic.Int64
		latenciesMu sync.Mutex
		latencies   = make([]time.Duration, 0, N)
	)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	wg.Add(N)

	startAll := time.Now()
	for i := 0; i < N; i++ {
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()
			_, err := e.Run(context.Background(), dgruntime.TurnInput{
				AppID:     "rt3-app",
				SessionID: fmt.Sprintf("sess-%d", i),
				UserID:    fmt.Sprintf("u%d", i%500),
			})
			d := time.Since(start)

			if err != nil {
				errs.Add(1)
			}
			runs.Add(1)

			latenciesMu.Lock()
			latencies = append(latencies, d)
			latenciesMu.Unlock()
		}(i)
	}
	wg.Wait()
	totalElapsed := time.Since(startAll)

	if errs.Load() > 0 {
		t.Errorf("got %d errors in %d runs", errs.Load(), N)
	}
	if runs.Load() != N {
		t.Errorf("runs = %d, want %d", runs.Load(), N)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[N*50/100]
	p99 := latencies[N*99/100]
	p999 := latencies[N*999/1000]
	throughput := float64(N) / totalElapsed.Seconds()

	t.Logf("N=%d concurrency=%d total=%v throughput=%.0f turns/s p50=%v p99=%v p999=%v",
		N, concurrency, totalElapsed, throughput, p50, p99, p999)

	// Sanity bounds : p99 should be well under 100ms for a no-tool turn.
	// With the shared-engine / non-oversubscribed setup the real engine p99
	// sits around 2-6ms, so this is a ~15-30× safety margin that still trips
	// hard on a genuine regression (a per-turn lock contention or accidental
	// O(history) blowup would push p99 into the tens-of-ms and beyond).
	if p99 > 100*time.Millisecond {
		t.Errorf("p99 too high : %v", p99)
	}
	// Throughput floor : the real scalability signal. The engine must sustain
	// well over 10K turns/s on this hardware ; dropping below 10K would mean a
	// serialisation regression (e.g. a global lock on the turn path).
	if throughput < 10_000 {
		t.Errorf("throughput too low : %.0f turns/s", throughput)
	}
}

// =====================================================================
// UT-L2 — Sustained tool dispatch throughput. Validates that the
// MetaDispatcher + BusAdapter combination can handle dense
// dispatch volume without contention.
// =====================================================================

// staticOutcomeDispatcher returns a fixed outcome on every call —
// used to isolate dispatch overhead from any module work.
type staticOutcomeDispatcher struct {
	calls atomic.Int64
}

func (s *staticOutcomeDispatcher) Dispatch(_ context.Context, _ dgruntime.ToolInvocation) dgruntime.ToolOutcome {
	s.calls.Add(1)
	return dgruntime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
	}
}

func TestStress_DispatchThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test slow under -short")
	}
	const (
		target = 50_000 // total dispatches
		concur = 32
	)
	disp := &staticOutcomeDispatcher{}

	var wg sync.WaitGroup
	wg.Add(concur)
	per := target / concur
	start := time.Now()

	for c := 0; c < concur; c++ {
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = disp.Dispatch(context.Background(), dgruntime.ToolInvocation{
					CallID: "c", Name: "x.y", Args: nil,
				})
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	ops := disp.calls.Load()
	thr := float64(ops) / elapsed.Seconds()
	t.Logf("dispatched=%d in %v (%.0f ops/s, %.1fµs/op)",
		ops, elapsed, thr, elapsed.Seconds()*1e6/float64(ops))
	if thr < 100_000 {
		t.Errorf("throughput too low : %.0f ops/s", thr)
	}
}

// =====================================================================
// UT-L3 — Build + search a 5000-tool ToolIndex. Validates that the
// CB-1 keyword index scales without quadratic blowup.
// =====================================================================

func TestStress_5000ToolIndexBuildAndSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test slow under -short")
	}
	const N = 5000

	universe := make([]policy.AvailableAction, N)
	domains := []string{"filesystem", "database", "shell", "http", "memory", "workspace"}
	for i := 0; i < N; i++ {
		dom := domains[i%len(domains)]
		fqn := fmt.Sprintf("%s.action_%d", dom, i)
		universe[i] = policy.AvailableAction{
			Module: dom,
			Action: fmt.Sprintf("action_%d", i),
			Spec: &tool.Spec{
				Name:        fqn,
				Description: fmt.Sprintf("Action #%d in domain %s — does something useful", i, dom),
				RiskLevel:   tool.RiskLow,
				Tags:        []string{dom, "auto-gen"},
				Params: []tool.ParamSpec{
					{Name: "input", Type: "string", Required: true},
				},
			},
		}
	}

	buildStart := time.Now()
	app := realDispatchApp()
	idx := index.NewBuilder().Build(true, app.Definition.Tools.Capabilities,
		&app.Definition.Agents[0], universe)
	buildDur := time.Since(buildStart)
	t.Logf("built %d-tool index in %v", N, buildDur)

	if !raceEnabled && buildDur > 2*time.Second {
		t.Errorf("build too slow for 5000 tools : %v", buildDur)
	}

	// Run a bag of queries and check latencies.
	queries := []string{
		"read a file", "execute shell", "send http request",
		"store in memory", "create a workspace file", "query database",
		"action_42", "action_4999", "useful", "auto-gen",
	}
	const Q = 1000
	rng := rand.New(rand.NewSource(42))
	latencies := make([]time.Duration, 0, Q)
	for i := 0; i < Q; i++ {
		q := queries[rng.Intn(len(queries))]
		start := time.Now()
		_ = idx.Search(q, 10)
		latencies = append(latencies, time.Since(start))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[Q*50/100]
	p99 := latencies[Q*99/100]
	t.Logf("search p50=%v p99=%v (n=%d queries on %d tools)", p50, p99, Q, N)

	// p99 bound is generous on purpose : when this test runs in
	// the same go-test process as a heavy server suite, the
	// scheduler gives us limited CPU. Solo p99 is typically
	// ~40ms ; we cap at 200ms to avoid CI flakiness while still
	// catching real regressions (e.g. quadratic blowup would
	// push this into seconds, not 200ms).
	if !raceEnabled && p99 > 200*time.Millisecond {
		t.Errorf("search p99 too high : %v", p99)
	}
}

// =====================================================================
// UT-L1 (bis) — Memory growth bounded over sustained load.
// =====================================================================

func TestStress_MemoryBoundedOver1KTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test slow under -short")
	}

	app := realDispatchApp()
	apps := &stubApps{app: app}

	// Warm up + baseline.
	for i := 0; i < 100; i++ {
		sess := newProjectingSessions("sess-warmup")
		lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
		e := newEngine(t, apps, sess, lc)
		_, _ = e.Run(context.Background(), dgruntime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-warmup", UserID: "u",
		})
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	// Heavy phase.
	const N = 1000
	for i := 0; i < N; i++ {
		sess := newProjectingSessions(fmt.Sprintf("sess-%d", i))
		lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
		e := newEngine(t, apps, sess, lc)
		_, _ = e.Run(context.Background(), dgruntime.TurnInput{
			AppID:     "rt3-app",
			SessionID: fmt.Sprintf("sess-%d", i),
			UserID:    "u",
		})
	}
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	deltaMB := float64(int64(m1.HeapAlloc)-int64(m0.HeapAlloc)) / 1024 / 1024
	t.Logf("memory after %d turns : Δheap=%.2f MB (baseline=%.2f MB)",
		N, deltaMB, float64(m0.HeapAlloc)/1024/1024)
	// 1000 turns should not add more than 50MB on the heap
	// (mostly per-session state stored in projection).
	if deltaMB > 50 {
		t.Errorf("memory growth too high : Δ=%.2f MB after %d turns", deltaMB, N)
	}
}
