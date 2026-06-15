package indexer

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestChaos_Lease_LockDBUnreachable_HaltsAllSyncs proves the documented severe
// failure mode: PgStore.Acquire returns ok=false when the lock DB is
// UNREACHABLE, which is INDISTINGUISHABLE from "held by another replica". So a
// transient lock-DB outage makes EVERY scheduled sync skip (LeaseSkipped climbs)
// with NO error surfaced — indexing silently halts cluster-wide until the lock
// DB returns. There is no degraded-mode alarm.
//
//	PG_CHAOS_URL=... PG_CHAOS_CONTAINER=pg-chaos \
//	  go test ./internal/indexer/ -run TestChaos_Lease_LockDBUnreachable_HaltsAllSyncs -v -timeout 120s
func TestChaos_Lease_LockDBUnreachable_HaltsAllSyncs(t *testing.T) {
	url := os.Getenv("PG_CHAOS_URL")
	cname := os.Getenv("PG_CHAOS_CONTAINER")
	if url == "" || cname == "" {
		t.Skip("set PG_CHAOS_URL and PG_CHAOS_CONTAINER")
	}
	ctx := context.Background()
	st, err := NewPgStore(ctx, url)
	if err != nil {
		t.Fatalf("pgstore: %v", err)
	}
	defer st.Close()

	// Baseline: lock DB up -> Acquire succeeds.
	rel, ok := st.Acquire(ctx, "src-key")
	if !ok {
		t.Fatal("baseline Acquire failed while lock DB is up")
	}
	rel()
	t.Log("baseline: Acquire OK while lock DB is up")

	// Kill the lock DB.
	if out, err := exec.Command("docker", "stop", "-t", "2", cname).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v: %s", err, out)
	}
	defer func() { _, _ = exec.Command("docker", "start", cname).CombinedOutput() }()
	t.Log("lock DB stopped; probing Acquire for several distinct sources")

	// Now Acquire must return ok=false for EVERY source — same signal as
	// "held elsewhere". A scheduler treating this as "skip, retry next tick"
	// silently halts all syncs with no failure surfaced.
	var skipped int
	keys := []string{"srcA", "srcB", "srcC", "srcD"}
	for _, k := range keys {
		actx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, ok := st.Acquire(actx, k)
		cancel()
		if !ok {
			skipped++
		}
	}
	t.Logf("with lock DB DOWN: %d/%d sources got ok=false (would be skipped, LeaseSkipped++, NO error)", skipped, len(keys))
	if skipped != len(keys) {
		t.Errorf("expected all %d sources to skip when lock DB unreachable, got %d", len(keys), skipped)
	}
	t.Logf("CONFIRMED: lock-DB outage makes Acquire return ok=false for ALL sources, indistinguishable from 'held by another replica'. The scheduler skips every sync (LeaseSkipped climbs) and surfaces NO error — silent cluster-wide indexing halt. cursor_pg.go:81-96 returns (noop,false) on both pool.Acquire error AND lock-held.")

	// Restart and confirm self-heal.
	if out, err := exec.Command("docker", "start", cname).CombinedOutput(); err != nil {
		t.Fatalf("docker start: %v: %s", err, out)
	}
	healed := false
	for i := 0; i < 40; i++ {
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		r, ok := st.Acquire(actx, "src-key")
		cancel()
		if ok {
			r()
			healed = true
			t.Logf("self-healed after %d attempts: Acquire works again once lock DB returns", i+1)
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !healed {
		t.Error("lease did NOT self-heal after lock DB restart")
	}
}
