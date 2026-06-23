package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/agent"
)

type fakeRunner struct {
	fn func(ctx context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error)
}

func (f fakeRunner) RunSubAgent(ctx context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
	return f.fn(ctx, spec)
}

func newMgr(fn func(ctx context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error)) *agent.Manager {
	m := agent.New(nil)
	m.AttachRunner(fakeRunner{fn: fn})
	return m
}

// TestSpawnWaitAndTelemetry : spawn returns a run id immediately, Wait collects
// the structured result, and the real-time telemetry the engine reports via the
// ctx Recorder is reflected in the snapshot.
func TestSpawnWaitAndTelemetry(t *testing.T) {
	m := newMgr(func(ctx context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		if r := dgruntime.RecorderFromContext(ctx); r != nil {
			r.AddLLMCall(10, 5)
			r.AddToolCall("")
			r.AddToolCall("")
		}
		return dgruntime.AgentResult{RunID: spec.RunID, AgentID: spec.AgentID, Content: "answer:" + spec.Task, Status: "completed"}, nil
	})

	id, err := m.Spawn(context.Background(), agent.SpawnRequest{AppID: "a", RootSession: "root", AgentID: "coding", Task: "build it"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.HasPrefix(id, "coding#") {
		t.Errorf("run id = %q, want coding#... prefix", id)
	}

	snap, err := m.Wait(context.Background(), "root", id, 2*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if snap.Status != "completed" || snap.Content != "answer:build it" {
		t.Errorf("result = %+v", snap)
	}
	if snap.LLMCalls != 1 || snap.TokensIn != 10 || snap.TokensOut != 5 {
		t.Errorf("token telemetry wrong: %+v", snap)
	}
	if snap.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2", snap.ToolCalls)
	}
}

// TestSpawnIsNonBlocking : Spawn must return immediately even when the agent
// runs forever — the caller never blocks.
func TestSpawnIsNonBlocking(t *testing.T) {
	release := make(chan struct{})
	m := newMgr(func(ctx context.Context, _ dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		<-release
		return dgruntime.AgentResult{Status: "completed"}, nil
	})
	start := time.Now()
	if _, err := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "x"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Spawn blocked for %s — must return immediately", d)
	}
	close(release)
}

// TestPanicIsIsolated : an agent whose runner panics is marked errored and never
// crashes the manager or a sibling agent.
func TestPanicIsIsolated(t *testing.T) {
	m := newMgr(func(_ context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		if spec.AgentID == "boom" {
			panic("kaboom")
		}
		return dgruntime.AgentResult{Content: "fine", Status: "completed"}, nil
	})
	bad, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "boom"})
	good, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "ok"})

	bs, _ := m.Wait(context.Background(), "r", bad, time.Second)
	if bs.Status != "errored" || !strings.Contains(bs.Error, "panic") {
		t.Errorf("panicking agent must be errored, got %+v", bs)
	}
	gs, _ := m.Wait(context.Background(), "r", good, time.Second)
	if gs.Status != "completed" || gs.Content != "fine" {
		t.Errorf("sibling must be unaffected by the panic, got %+v", gs)
	}
}

// TestDepthGuard : delegation depth is capped — a buggy agent can't recurse
// forever.
func TestDepthGuard(t *testing.T) {
	m := newMgr(func(_ context.Context, _ dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		return dgruntime.AgentResult{Status: "completed"}, nil
	})
	m.MaxDepth = 2

	a, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "a"})                       // depth 0
	b, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "b", ParentRunID: a})       // depth 1
	c, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "c", ParentRunID: b})       // depth 2
	if _, err := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "d", ParentRunID: c}); // depth 3 > 2
	!errors.Is(err, agent.ErrDepth) {
		t.Errorf("spawn beyond max depth must fail with ErrDepth, got %v", err)
	}
}

// TestBudgetGuard : a per-root agent budget stops a fork-bomb.
func TestBudgetGuard(t *testing.T) {
	m := newMgr(func(_ context.Context, _ dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		return dgruntime.AgentResult{Status: "completed"}, nil
	})
	m.MaxAgentsPerRoot = 2
	if _, err := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "c"}); !errors.Is(err, agent.ErrBudget) {
		t.Errorf("spawn beyond budget must fail with ErrBudget, got %v", err)
	}
}

// TestCancelTree : cancelling a parent cancels its whole subtree via the ctx
// tree.
func TestCancelTree(t *testing.T) {
	m := newMgr(func(ctx context.Context, _ dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		<-ctx.Done() // block until cancelled
		return dgruntime.AgentResult{Status: "cancelled"}, ctx.Err()
	})
	parent, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "parent"})
	child, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "child", ParentRunID: parent})

	if err := m.Cancel("r", parent); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	cs, _ := m.Wait(context.Background(), "r", child, time.Second)
	if cs.Status != "cancelled" {
		t.Errorf("cancelling the parent must cancel the child, got %q", cs.Status)
	}
}

// TestCancelAll : a session abort must halt the WHOLE delegated tree — every
// agent under the root, independent contexts and all. Each unwinds to
// "cancelled".
func TestCancelAll(t *testing.T) {
	m := newMgr(func(ctx context.Context, _ dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		<-ctx.Done()
		return dgruntime.AgentResult{Status: "cancelled"}, ctx.Err()
	})
	// Two agents under root "r" (one nested), one under a different root "other".
	a, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "a"})
	b, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "b", ParentRunID: a})
	keep, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "other", AgentID: "keep"})

	if n := m.CancelAll("r"); n != 2 {
		t.Fatalf("CancelAll signalled %d agents, want 2", n)
	}
	for _, id := range []string{a, b} {
		s, _ := m.Wait(context.Background(), "r", id, time.Second)
		if s.Status != "cancelled" {
			t.Errorf("agent %s status = %q, want cancelled", id, s.Status)
		}
	}

	// A DIFFERENT root is untouched — abort is strictly per-session.
	if s, _ := m.Status("other", keep); s.Status != "running" {
		t.Errorf("agent in another session must survive, got %q", s.Status)
	}

	// CancelAll on an unknown root is a harmless no-op.
	if n := m.CancelAll("does-not-exist"); n != 0 {
		t.Errorf("CancelAll on unknown root = %d, want 0", n)
	}
	m.CancelAll("other") // unblock the surviving agent's goroutine
}

// TestNoAgentBlocksAnother : a slow agent must never block a fast one — true
// independence. The fast agent completes long before the slow one.
func TestNoAgentBlocksAnother(t *testing.T) {
	m := newMgr(func(ctx context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		if spec.AgentID == "slow" {
			time.Sleep(500 * time.Millisecond)
		}
		return dgruntime.AgentResult{Content: spec.AgentID, Status: "completed"}, nil
	})
	_, _ = m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "slow"})
	fast, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "r", AgentID: "fast"})

	start := time.Now()
	fs, err := m.Wait(context.Background(), "r", fast, time.Second)
	if err != nil || fs.Status != "completed" {
		t.Fatalf("fast agent failed: %+v %v", fs, err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("fast agent was blocked by the slow one (%s) — independence broken", d)
	}
}

// TestNestedWaitIsDeadlockFree : an agent that spawns a child and WAITS for it,
// nested several levels deep, must complete. A parent waiting on a child holds
// nothing scarce (the core invariant), so the tree drains without deadlock.
func TestNestedWaitIsDeadlockFree(t *testing.T) {
	m := agent.New(nil)
	m.MaxDepth = 10
	m.AttachRunner(fakeRunner{fn: func(ctx context.Context, spec dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		if spec.Depth < 4 {
			cid, err := m.Spawn(ctx, agent.SpawnRequest{RootSession: "root", AgentID: "c", ParentRunID: spec.RunID})
			if err != nil {
				return dgruntime.AgentResult{}, err
			}
			cs, err := m.Wait(ctx, "root", cid, 5*time.Second)
			if err != nil {
				return dgruntime.AgentResult{}, err
			}
			return dgruntime.AgentResult{Content: "->" + cs.Content, Status: "completed"}, nil
		}
		return dgruntime.AgentResult{Content: "leaf", Status: "completed"}, nil
	}})

	id, _ := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "root", AgentID: "root"})
	s, err := m.Wait(context.Background(), "root", id, 10*time.Second)
	if err != nil || s.Status != "completed" {
		t.Fatalf("nested wait deadlocked or failed: %+v %v", s, err)
	}
	if !strings.HasSuffix(s.Content, "leaf") {
		t.Errorf("nested result not threaded through: %q", s.Content)
	}
}

// TestStress_ThousandsOfConcurrentAgents : prove the registry + scheduling hold
// thousands of agents at once with no blocking and no lost agents.
func TestStress_ThousandsOfConcurrentAgents(t *testing.T) {
	m := newMgr(func(ctx context.Context, _ dgruntime.SubAgentSpec) (dgruntime.AgentResult, error) {
		if r := dgruntime.RecorderFromContext(ctx); r != nil {
			r.AddLLMCall(1, 1)
		}
		return dgruntime.AgentResult{Status: "completed"}, nil
	})
	const n = 3000
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id, err := m.Spawn(context.Background(), agent.SpawnRequest{RootSession: "root", AgentID: "w"})
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		ids[i] = id
	}
	snaps, err := m.WaitAll(context.Background(), "root", ids, 30*time.Second)
	if err != nil {
		t.Fatalf("WaitAll: %v", err)
	}
	for i, s := range snaps {
		if s.Status != "completed" {
			t.Fatalf("agent %d not completed: %+v", i, s)
		}
	}
	if got := len(m.List("root")); got != n {
		t.Errorf("list = %d, want %d", got, n)
	}
}
