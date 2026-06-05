package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"
)

// TestManager_Spawn_OverUnixSocket spawns a REAL worker subprocess bound to
// an AF_UNIX socket and drives the full daemon→worker path over it: stdout
// address discovery, the unix dial, and a health RPC. Proves the UDS
// transport (the ~2.8× lower-latency path) end-to-end on this platform. If
// AF_UNIX is unavailable, the worker fails to bind and the spawn errors —
// caught and reported, not silently skipped.
func TestManager_Spawn_OverUnixSocket(t *testing.T) {
	exe := buildDummyWorker(t)
	sock := filepath.Join(t.TempDir(), "w.sock")

	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{
		Kind:         "dummy",
		Binary:       exe,
		Count:        1,
		StartTimeout: 10 * time.Second,
		Env:          map[string]string{EnvBindKey: "unix:" + sock},
	}); err != nil {
		t.Fatalf("spawn over unix socket: %v", err)
	}

	pool := m.Pool("dummy")
	if len(pool) != 1 {
		t.Fatalf("pool size: %d", len(pool))
	}
	if !strings.HasPrefix(pool[0].Address, "unix:") {
		t.Fatalf("advertised address = %q, want unix: prefix", pool[0].Address)
	}

	c, err := m.Pick(ctx, "dummy")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	st, err := HealthCheck(ctx, c, "")
	if err != nil {
		t.Fatalf("health over unix socket: %v", err)
	}
	if st != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("status: %s", st)
	}
}

// TestManager_Spawn_UnixSocketDir_NoCollision proves a pool of N instances
// sharing one "unix:<dir>/" env each bind a UNIQUE socket inside the dir —
// the production shape for a multi-instance worker pool.
func TestManager_Spawn_UnixSocketDir_NoCollision(t *testing.T) {
	exe := buildDummyWorker(t)
	dir := t.TempDir() + string('/')

	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{
		Kind:         "dummy",
		Binary:       exe,
		Count:        2,
		StartTimeout: 10 * time.Second,
		Env:          map[string]string{EnvBindKey: "unix:" + dir},
	}); err != nil {
		t.Fatalf("spawn pool over unix dir: %v", err)
	}

	pool := m.Pool("dummy")
	if len(pool) != 2 {
		t.Fatalf("pool size: %d", len(pool))
	}
	seen := map[string]bool{}
	for _, h := range pool {
		if !strings.HasPrefix(h.Address, "unix:") {
			t.Fatalf("address = %q, want unix: prefix", h.Address)
		}
		if seen[h.Address] {
			t.Fatalf("two instances bound the same socket: %q", h.Address)
		}
		seen[h.Address] = true
	}
}
