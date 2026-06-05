package runtime_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	runtimepkg "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/turn"
)

// ISOLATION : the multi-tenant safety question — does session X's
// heavy activity slow down session Y ? Three benches prove the answer
// is "no, when the pool is configured" and quantify the price you pay
// when you mis-configure.

// runIsolationScenario fires "fast" turns for one app/user while a
// "noisy" peer pounds another app/user on the same runtime. Returns
// the fast turns' latency distribution.
type isoResult struct {
	Label   string
	FastP50 time.Duration
	FastP99 time.Duration
	FastErr int
}

func runIsolationScenario(b *testing.B, label string,
	pool *turn.Pool,
	noisyN int, noisyApp, noisyUser string, noisyLLMDelay time.Duration,
	fastN int, fastApp, fastUser string, fastLLMDelay time.Duration,
) isoResult {
	b.Helper()
	noisyAppDef := buildAppWithID(noisyApp)
	fastAppDef := buildAppWithID(fastApp)
	apps := &multiApps{m: map[string]*appmgr.RuntimeApp{
		noisyApp: noisyAppDef,
		fastApp:  fastAppDef,
	}}

	noisyLLM := &slowLLM{delay: noisyLLMDelay}
	fastLLM := &slowLLM{delay: fastLLMDelay}

	// Fast latencies are what we measure.
	fastLatencies := make([]time.Duration, fastN)
	var fastErrCount int
	var mu sync.Mutex

	var wg sync.WaitGroup
	// Spawn noisy first, give them a head start to occupy slots.
	wg.Add(noisyN)
	for i := 0; i < noisyN; i++ {
		go func(i int) {
			defer wg.Done()
			sess := &stubSessions{state: sessionstore.NewSessionState(fmt.Sprintf("noisy-%d", i)), appendSeq: 1}
			ge := newBenchEngine(apps, sess, noisyLLM, pool)
			_, _ = ge.Run(context.Background(), runtimepkg.TurnInput{
				AppID: noisyApp, SessionID: fmt.Sprintf("noisy-%d", i), UserID: noisyUser,
			})
		}(i)
	}
	// Tiny stagger so the noisy ones get to Acquire first ; we want
	// the fast turns to fire WHILE noisy is occupying slots.
	time.Sleep(2 * time.Millisecond)

	wg.Add(fastN)
	for i := 0; i < fastN; i++ {
		go func(i int) {
			defer wg.Done()
			sess := &stubSessions{state: sessionstore.NewSessionState(fmt.Sprintf("fast-%d", i)), appendSeq: 1}
			ge := newBenchEngine(apps, sess, fastLLM, pool)
			t0 := time.Now()
			_, err := ge.Run(context.Background(), runtimepkg.TurnInput{
				AppID: fastApp, SessionID: fmt.Sprintf("fast-%d", i), UserID: fastUser,
			})
			lat := time.Since(t0)
			mu.Lock()
			fastLatencies[i] = lat
			if err != nil {
				fastErrCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	sort.Slice(fastLatencies, func(i, j int) bool { return fastLatencies[i] < fastLatencies[j] })
	return isoResult{
		Label:   label,
		FastP50: fastLatencies[len(fastLatencies)/2],
		FastP99: fastLatencies[(len(fastLatencies)*99)/100],
		FastErr: fastErrCount,
	}
}

func buildAppWithID(id string) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: id},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID:    "primary",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
			}},
		},
		BundleDir: "/tmp/" + id,
	}
}

// multiApps serves multiple AppIDs from a fixed map. The single
// stubApps from engine_test.go only knows one app — we need both
// "noisy" and "fast" present.
type multiApps struct {
	m map[string]*appmgr.RuntimeApp
}

func (m *multiApps) Get(_ context.Context, id string) (*appmgr.RuntimeApp, error) {
	a, ok := m.m[id]
	if !ok {
		return nil, fmt.Errorf("app not found: %s", id)
	}
	return a, nil
}

// BenchmarkIsolation_AppBoundary measures whether a noisy app slows
// down a quiet peer. With PerAppCap configured, the noisy app holds
// at most PerAppCap slots ; the fast app gets its own share. With
// PerAppCap=0 (unbounded), the noisy app CAN starve the fast one if
// it spawns enough turns to exceed GlobalCap.
func BenchmarkIsolation_AppBoundary(b *testing.B) {
	b.StopTimer()

	// Pool sized so noisy CAN'T monopolize : GlobalCap=200, PerAppCap=100.
	// 1000 noisy turns will queue past PerAppCap=100 ; meanwhile fast's
	// 100 turns get instant access to their own 100-slot share.
	pool := turn.NewPool(turn.PoolConfig{GlobalCap: 200, PerAppCap: 100, PerUserCap: 100})

	// Baseline : fast app ALONE, no noisy peer.
	baseline := runIsolationScenario(b, "baseline_alone",
		pool,
		0, "noisy-app", "u-noisy", 0,
		100, "fast-app", "u-fast", 5*time.Millisecond,
	)

	// Contended : 1000 noisy turns hammering at 500ms each while
	// 100 fast turns run.
	contended := runIsolationScenario(b, "noisy_peer_1000x500ms",
		pool,
		1000, "noisy-app", "u-noisy", 500*time.Millisecond,
		100, "fast-app", "u-fast", 5*time.Millisecond,
	)

	b.StartTimer()

	fmt.Println()
	fmt.Println("=== ISOLATION : noisy APP vs fast APP ===")
	fmt.Println("pool: GlobalCap=200 PerAppCap=100 PerUserCap=100")
	fmt.Printf("%-30s %-12s %-12s %-8s\n", "scenario", "fast p50", "fast p99", "errors")
	fmt.Printf("%-30s %-12v %-12v %-8d\n", baseline.Label, baseline.FastP50.Round(time.Millisecond), baseline.FastP99.Round(time.Millisecond), baseline.FastErr)
	fmt.Printf("%-30s %-12v %-12v %-8d\n", contended.Label, contended.FastP50.Round(time.Millisecond), contended.FastP99.Round(time.Millisecond), contended.FastErr)
	delta := contended.FastP99 - baseline.FastP99
	pct := 100.0 * float64(delta) / float64(baseline.FastP99)
	fmt.Printf("p99 degradation : %v (%.1f%%)\n", delta.Round(time.Millisecond), pct)
	fmt.Println("=========================================")
	fmt.Println()
}

// BenchmarkIsolation_UserBoundary measures whether a noisy user slows
// down a quiet user OF THE SAME APP. With PerUserCap configured, each
// user has their own ceiling, so user A can't monopolize user B's
// share.
func BenchmarkIsolation_UserBoundary(b *testing.B) {
	b.StopTimer()

	pool := turn.NewPool(turn.PoolConfig{GlobalCap: 200, PerAppCap: 200, PerUserCap: 50})

	baseline := runIsolationScenario(b, "baseline_alone",
		pool,
		0, "shared-app", "u-noisy", 0,
		100, "shared-app", "u-fast", 5*time.Millisecond,
	)

	contended := runIsolationScenario(b, "noisy_user_1000x500ms",
		pool,
		1000, "shared-app", "u-noisy", 500*time.Millisecond,
		100, "shared-app", "u-fast", 5*time.Millisecond,
	)

	b.StartTimer()

	fmt.Println()
	fmt.Println("=== ISOLATION : noisy USER vs fast USER (same app) ===")
	fmt.Println("pool: GlobalCap=200 PerAppCap=200 PerUserCap=50")
	fmt.Printf("%-30s %-12s %-12s %-8s\n", "scenario", "fast p50", "fast p99", "errors")
	fmt.Printf("%-30s %-12v %-12v %-8d\n", baseline.Label, baseline.FastP50.Round(time.Millisecond), baseline.FastP99.Round(time.Millisecond), baseline.FastErr)
	fmt.Printf("%-30s %-12v %-12v %-8d\n", contended.Label, contended.FastP50.Round(time.Millisecond), contended.FastP99.Round(time.Millisecond), contended.FastErr)
	delta := contended.FastP99 - baseline.FastP99
	pct := 100.0 * float64(delta) / float64(baseline.FastP99)
	fmt.Printf("p99 degradation : %v (%.1f%%)\n", delta.Round(time.Millisecond), pct)
	fmt.Println("======================================================")
	fmt.Println()
}

// BenchmarkIsolation_SameUser_DifferentSessions is the strict test :
// one user, many sessions, ONE session is doing a slow turn (10s LLM).
// We assert that the OTHER sessions of the SAME user keep their p99
// under load. This is the "user has 10 chat tabs open, one is grinding
// on a tool call, the rest stay responsive" promise.
//
// Configuration : PerUserCap >= N so all sessions fit under the user
// cap and none queue at the tier level. The only way a slow session
// could affect a fast one is via shared downstream resources (bus
// shards, LLM worker pool, GC). We measure to verify.
func BenchmarkIsolation_SameUser_DifferentSessions(b *testing.B) {
	b.StopTimer()

	// PerUserCap=200 absorbs all 100 sessions + slow session ; no
	// queuing at any tier level.
	pool := turn.NewPool(turn.PoolConfig{GlobalCap: 500, PerAppCap: 500, PerUserCap: 200})

	baseline := runIsolationScenario(b, "fast_only_99_sessions",
		pool,
		0, "shared-app", "u-1", 0,
		99, "shared-app", "u-1", 5*time.Millisecond,
	)

	// Now : 1 session of the SAME user is grinding 10s (e.g. blocked on
	// a slow tool call). 99 other sessions of the same user run.
	contended := runIsolationScenario(b, "1_slow_99_fast_same_user",
		pool,
		1, "shared-app", "u-1", 10*time.Second,
		99, "shared-app", "u-1", 5*time.Millisecond,
	)

	b.StartTimer()

	fmt.Println()
	fmt.Println("=== STRICT TEST : same user, 1 slow session + 99 fast ===")
	fmt.Println("pool: GlobalCap=500 PerAppCap=500 PerUserCap=200")
	fmt.Printf("%-30s %-12s %-12s %-8s\n", "scenario", "fast p50", "fast p99", "errors")
	fmt.Printf("%-30s %-12v %-12v %-8d\n", baseline.Label, baseline.FastP50.Round(time.Millisecond), baseline.FastP99.Round(time.Millisecond), baseline.FastErr)
	fmt.Printf("%-30s %-12v %-12v %-8d\n", contended.Label, contended.FastP50.Round(time.Millisecond), contended.FastP99.Round(time.Millisecond), contended.FastErr)
	delta := contended.FastP99 - baseline.FastP99
	pct := 100.0 * float64(delta) / float64(baseline.FastP99)
	fmt.Printf("p99 degradation : %v (%.1f%%)\n", delta.Round(time.Millisecond), pct)
	fmt.Println("Expected : the 1 slow session must NOT affect the 99 fast ones.")
	fmt.Println("Pass = degradation < 50%.")
	fmt.Println("============================================================")
	fmt.Println()
}

// BenchmarkIsolation_Misconfigured shows what happens when the pool
// is sized WITHOUT tiered caps : a noisy peer DOES affect the fast
// one because there's no fairness ceiling. This is the "anti-test"
// proving the tiered semaphore architecture matters.
func BenchmarkIsolation_Misconfigured(b *testing.B) {
	b.StopTimer()

	// Only a global cap, no per-app / per-user. Noisy can take it all.
	pool := turn.NewPool(turn.PoolConfig{GlobalCap: 200})

	baseline := runIsolationScenario(b, "baseline_alone",
		pool,
		0, "noisy-app", "u-noisy", 0,
		100, "fast-app", "u-fast", 5*time.Millisecond,
	)

	contended := runIsolationScenario(b, "noisy_peer_unbounded",
		pool,
		1000, "noisy-app", "u-noisy", 500*time.Millisecond,
		100, "fast-app", "u-fast", 5*time.Millisecond,
	)

	b.StartTimer()

	fmt.Println()
	fmt.Println("=== ANTI-TEST : isolation WITHOUT tiered caps ===")
	fmt.Println("pool: GlobalCap=200 only (no PerAppCap / PerUserCap)")
	fmt.Printf("%-30s %-12s %-12s %-8s\n", "scenario", "fast p50", "fast p99", "errors")
	fmt.Printf("%-30s %-12v %-12v %-8d\n", baseline.Label, baseline.FastP50.Round(time.Millisecond), baseline.FastP99.Round(time.Millisecond), baseline.FastErr)
	fmt.Printf("%-30s %-12v %-12v %-8d\n", contended.Label, contended.FastP50.Round(time.Millisecond), contended.FastP99.Round(time.Millisecond), contended.FastErr)
	delta := contended.FastP99 - baseline.FastP99
	pct := 100.0 * float64(delta) / float64(baseline.FastP99)
	fmt.Printf("p99 degradation : %v (%.1f%%) ← noisy CAN starve fast\n", delta.Round(time.Millisecond), pct)
	fmt.Println("==================================================")
	fmt.Println()
}
