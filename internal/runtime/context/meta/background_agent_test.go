package meta_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
)

// countingAgents counts spawns and echoes the task in the run id, so a
// parallel fan-out's results stay distinguishable per action.
type countingAgents struct{ spawns atomic.Int64 }

func (c *countingAgents) Spawn(_ context.Context, req meta.AgentSpawnRequest) (string, error) {
	c.spawns.Add(1)
	return "run-" + req.Task, nil
}
func (c *countingAgents) Wait(_ context.Context, _, runID string, _ float64) (meta.AgentSnapshot, error) {
	return meta.AgentSnapshot{RunID: runID, Status: "completed", Content: runID}, nil
}
func (c *countingAgents) WaitAll(_ context.Context, _ string, ids []string, _ float64) ([]meta.AgentSnapshot, error) {
	out := make([]meta.AgentSnapshot, len(ids))
	for i, id := range ids {
		out[i] = meta.AgentSnapshot{RunID: id, Status: "completed", Content: id}
	}
	return out, nil
}
func (c *countingAgents) Status(_, runID string) (meta.AgentSnapshot, error) {
	return meta.AgentSnapshot{RunID: runID, Status: "running"}, nil
}
func (c *countingAgents) List(_ string) []meta.AgentSnapshot { return nil }
func (c *countingAgents) Cancel(_, _ string) error           { return nil }
func (c *countingAgents) SpawnBatch(ctx context.Context, reqs []meta.AgentSpawnRequest) ([]string, error) {
	ids := make([]string, len(reqs))
	for i, r := range reqs {
		id, err := c.Spawn(ctx, r)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

// TestBackgroundRun_AgentTarget_TransformsToDelegation : when background_run is
// asked to launch the `agent` delegation tool, it must NOT wrap it in a
// background task (whose task_id can't be correlated to the run). It transforms
// into a sub-agent spawn and returns the AGENT's run_id, so the caller collects
// it via agent(wait=true, run_id=...). "background_run → agent". It must work
// WITHOUT a BackgroundManager (it routes to Agents, not Background).
func TestBackgroundRun_AgentTarget_TransformsToDelegation(t *testing.T) {
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator} // NOTE: no Background wired

	out := m.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{
			"name":   "agent",
			"params": map[string]any{"agent": "researcher", "task": "analyse rapport1.txt"},
		},
		AppID: "app", AgentID: "coordinator", AgentRunID: "main", SessionID: "root1",
	})

	if out.Status == "errored" {
		t.Fatalf("background_run→agent errored: %s", out.Error)
	}
	body := ""
	if len(out.Parts) > 0 {
		body = out.Parts[0].Text
	}
	// Returns the AGENT run id (fakeAgents.Spawn → "coding#deadbeef"), NOT a task_id.
	if !strings.Contains(body, "coding#deadbeef") {
		t.Errorf("must return the agent run_id, got %q", body)
	}
	if strings.Contains(body, "task_id") {
		t.Errorf("must NOT return a background task_id for an agent target, got %q", body)
	}
	// The spawn was forwarded with the right target + parent + root.
	if f.lastSpawn.AgentID != "researcher" || f.lastSpawn.Task != "analyse rapport1.txt" {
		t.Errorf("spawn not forwarded from background_run params: %+v", f.lastSpawn)
	}
	if f.lastSpawn.ParentRunID != "main" || f.lastSpawn.RootSession != "root1" {
		t.Errorf("spawn lineage wrong: parent=%q root=%q", f.lastSpawn.ParentRunID, f.lastSpawn.RootSession)
	}
}

// TestBackgroundRun_AgentTarget_ForcesAsync : even if the model passes wait=true
// in the params, the background→agent path strips it so it returns a run_id
// immediately (background semantics) instead of blocking on the sub-agent.
func TestBackgroundRun_AgentTarget_ForcesAsync(t *testing.T) {
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}

	out := m.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{
			"name":   "agent",
			"params": map[string]any{"agent": "researcher", "task": "x", "wait": true},
		},
		AppID: "app", AgentID: "coordinator", AgentRunID: "", SessionID: "root1",
	})
	if out.Status == "errored" {
		t.Fatalf("errored: %s", out.Error)
	}
	body := ""
	if len(out.Parts) > 0 {
		body = out.Parts[0].Text
	}
	// "running" status proves it spawned async (mode-1 no-wait) instead of
	// blocking for the finished snapshot (which would say "completed").
	if !strings.Contains(body, "coding#deadbeef") || !strings.Contains(body, "running") {
		t.Errorf("background→agent must spawn async and return {run_id, running}, got %q", body)
	}
}

// TestRunParallel_AgentActions_DelegateInParallel : run_parallel with several
// `agent` actions is a legit way to fan out delegations — each action routes
// through Dispatch to handleAgent, so the sub-agents run concurrently and their
// results come back in input order. This locks the "parallel delegation works"
// guarantee (the model's instinct was correct).
func TestRunParallel_AgentActions_DelegateInParallel(t *testing.T) {
	f := &countingAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}

	out := m.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			map[string]any{"name": "agent", "params": map[string]any{"agent": "researcher", "task": "rapport1", "wait": true}},
			map[string]any{"name": "agent", "params": map[string]any{"agent": "researcher", "task": "rapport2", "wait": true}},
			map[string]any{"name": "agent", "params": map[string]any{"agent": "researcher", "task": "rapport3", "wait": true}},
		}},
		AppID: "app", AgentID: "coordinator", AgentRunID: "main", SessionID: "root1",
	})
	if out.Status == "errored" {
		t.Fatalf("run_parallel of agent actions errored: %s", out.Error)
	}
	if f.spawns.Load() != 3 {
		t.Errorf("expected 3 parallel delegations, got %d", f.spawns.Load())
	}
	body := ""
	if len(out.Parts) > 0 {
		body = out.Parts[0].Text
	}
	// All three sub-agent results must be present (input order preserved).
	for _, want := range []string{"rapport1", "rapport2", "rapport3"} {
		if !strings.Contains(body, want) {
			t.Errorf("parallel delegation lost result for %q:\n%s", want, body)
		}
	}
}

// TestBackgroundRun_NonAgentTarget_StillNeedsBackground : a normal (non-agent)
// launch still goes through the BackgroundManager — the transform is scoped to
// the agent tool only. With no Background wired, it reports "not wired".
func TestBackgroundRun_NonAgentTarget_StillNeedsBackground(t *testing.T) {
	m := &meta.MetaDispatcher{Agents: &fakeAgents{}} // no Background

	out := m.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{
			"name":   "database.sql",
			"params": map[string]any{"query": "SELECT 1"},
		},
		AppID: "app", AgentID: "main", SessionID: "root1",
	})
	if out.Status != "errored" || !strings.Contains(out.Error, "not wired") {
		t.Errorf("non-agent target must require a BackgroundManager, got status=%s err=%q", out.Status, out.Error)
	}
}
