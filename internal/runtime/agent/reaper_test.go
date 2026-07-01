package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
)

type reaperRunner struct {
	block <-chan struct{} // if non-nil, RunSubAgent waits on it before returning
}

func (r reaperRunner) RunSubAgent(ctx context.Context, _ runtime.SubAgentSpec) (runtime.AgentResult, error) {
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
		}
	}
	return runtime.AgentResult{Content: "ok"}, nil
}

func rootCount(m *Manager) int {
	n := 0
	m.roots.Range(func(_, _ any) bool { n++; return true })
	return n
}

func TestManager_ReapsTerminalAgentsAndEmptyRoots(t *testing.T) {
	var nowNano atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	nowNano.Store(base.UnixNano())

	m := New(nil)
	m.now = func() time.Time { return time.Unix(0, nowNano.Load()) }
	m.RetainCompleted = time.Minute
	m.AttachRunner(reaperRunner{})

	id, err := m.Spawn(context.Background(), SpawnRequest{RootSession: "root", AgentID: "a"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := m.Wait(context.Background(), "root", id, 2*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Within the retention window : the terminal agent is kept.
	m.reapAll()
	if got := len(m.List("root")); got != 1 {
		t.Fatalf("inside retention: List=%d, want 1 (reaped too early)", got)
	}

	// Past the retention window : the agent is reaped and its now-empty root
	// table is removed from the registry.
	nowNano.Store(base.Add(2 * time.Minute).UnixNano())
	m.reapAll()
	if got := len(m.List("root")); got != 0 {
		t.Fatalf("past retention: List=%d, want 0 (agent not reaped)", got)
	}
	if got := rootCount(m); got != 0 {
		t.Fatalf("past retention: roots=%d, want 0 (empty root not removed)", got)
	}
}

func TestManager_ReaperKeepsRunningAgents(t *testing.T) {
	var nowNano atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	nowNano.Store(base.UnixNano())

	m := New(nil)
	m.now = func() time.Time { return time.Unix(0, nowNano.Load()) }
	m.RetainCompleted = time.Minute

	release := make(chan struct{})
	m.AttachRunner(reaperRunner{block: release})

	id, err := m.Spawn(context.Background(), SpawnRequest{RootSession: "root", AgentID: "a"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Advance well past the retention window while the agent is still running :
	// a running agent (endedNano == 0) must never be reaped.
	nowNano.Store(base.Add(time.Hour).UnixNano())
	m.reapAll()
	if got := len(m.List("root")); got != 1 {
		t.Fatalf("running agent reaped: List=%d, want 1", got)
	}

	// Let it finish, then it becomes eligible.
	close(release)
	if _, err := m.Wait(context.Background(), "root", id, 2*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	nowNano.Store(base.Add(2 * time.Hour).UnixNano())
	m.reapAll()
	if got := rootCount(m); got != 0 {
		t.Fatalf("after completion: roots=%d, want 0", got)
	}
}
