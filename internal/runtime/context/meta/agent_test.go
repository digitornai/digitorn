package meta_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
)

// allowCoordinator is the test stand-in for the production CoordinatorLookup
// (which returns true for coordinator-role agents). The dispatcher now fails
// CLOSED when no lookup is wired, so tests that exercise delegation mechanics
// must declare an authorised caller explicitly.
func allowCoordinator(string, string) bool { return true }

type fakeAgents struct {
	lastSpawn meta.AgentSpawnRequest
	cancelled string
	listed    []meta.AgentSnapshot
	waitCalls int
}

func (f *fakeAgents) Spawn(_ context.Context, req meta.AgentSpawnRequest) (string, error) {
	f.lastSpawn = req
	return "coding#deadbeef", nil
}
func (f *fakeAgents) Wait(_ context.Context, _, runID string, _ float64) (meta.AgentSnapshot, error) {
	f.waitCalls++
	return meta.AgentSnapshot{RunID: runID, Status: "completed", Content: "sub done"}, nil
}
func (f *fakeAgents) WaitAll(_ context.Context, _ string, ids []string, _ float64) ([]meta.AgentSnapshot, error) {
	out := make([]meta.AgentSnapshot, len(ids))
	for i, id := range ids {
		out[i] = meta.AgentSnapshot{RunID: id, Status: "completed"}
	}
	return out, nil
}
func (f *fakeAgents) Status(_, runID string) (meta.AgentSnapshot, error) {
	return meta.AgentSnapshot{RunID: runID, Status: "running"}, nil
}
func (f *fakeAgents) List(_ string) []meta.AgentSnapshot { return f.listed }
func (f *fakeAgents) SpawnBatch(ctx context.Context, reqs []meta.AgentSpawnRequest) ([]string, error) {
	ids := make([]string, len(reqs))
	for i, r := range reqs {
		id, err := f.Spawn(ctx, r)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (f *fakeAgents) Cancel(_, runID string) error {
	f.cancelled = runID
	return nil
}
func (f *fakeAgents) CancelTree(_, _ string) int { return 0 }

func agentCall(args map[string]any, session, agentRunID string) runtime.ToolInvocation {
	return runtime.ToolInvocation{
		Name: "agent_spawn.agent", Args: args,
		AppID: "app", AgentID: "main", AgentRunID: agentRunID, SessionID: session,
	}
}

func TestAgentTool_Spawn(t *testing.T) {
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}
	out := m.Dispatch(context.Background(), agentCall(
		map[string]any{"agent": "coding", "task": "build X", "memory_seed": "goal: ship"}, "root1", "main"))
	if out.Status != "completed" {
		t.Fatalf("spawn failed: %s", out.Error)
	}
	if len(out.Parts) == 0 || !strings.Contains(out.Parts[0].Text, "coding#deadbeef") {
		t.Errorf("spawn must return the run id, got %+v", out.Parts)
	}
	if f.lastSpawn.AgentID != "coding" || f.lastSpawn.Task != "build X" || f.lastSpawn.MemorySeed != "goal: ship" {
		t.Errorf("spawn request not forwarded: %+v", f.lastSpawn)
	}
	if f.lastSpawn.ParentRunID != "main" {
		t.Errorf("parent run id must be the caller's, got %q", f.lastSpawn.ParentRunID)
	}
	if f.lastSpawn.RootSession != "root1" {
		t.Errorf("root session = %q, want root1", f.lastSpawn.RootSession)
	}
}

// TestAgentTool_SpawnDefaultsNonBlocking locks the non-blocking default: a
// delegation with NO wait flag must spawn and return the run_id IMMEDIATELY
// (status running) WITHOUT ever calling Wait — so the coordinator keeps working
// while the child runs instead of freezing its loop until the child finishes.
// Regression for "the parent loop blocks on delegate".
func TestAgentTool_SpawnDefaultsNonBlocking(t *testing.T) {
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}
	out := m.Dispatch(context.Background(), agentCall(
		map[string]any{"agent": "explore", "task": "map the subsystem"}, "root1", "main"))
	if out.Status != "completed" {
		t.Fatalf("non-blocking spawn errored: %s", out.Error)
	}
	if f.waitCalls != 0 {
		t.Fatalf("spawn without wait must NOT block on Wait (called %d times)", f.waitCalls)
	}
	if len(out.Parts) == 0 ||
		!strings.Contains(out.Parts[0].Text, "coding#deadbeef") ||
		!strings.Contains(out.Parts[0].Text, "running") {
		t.Errorf("non-blocking spawn must return run_id + running status, got %+v", out.Parts)
	}
	if strings.Contains(out.Parts[0].Text, "sub done") {
		t.Error("non-blocking spawn must NOT return the child's finished content")
	}
}

func TestAgentTool_SpawnAndWait(t *testing.T) {
	// The primary single-call delegation : a target with wait:true must
	// SPAWN then block and return the child's finished snapshot (with the
	// answer), NOT misroute to the pure-wait branch and demand a run_id.
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}
	out := m.Dispatch(context.Background(), agentCall(
		map[string]any{"agent": "researcher", "task": "capital of France?", "wait": true}, "root1", "main"))
	if out.Status != "completed" {
		t.Fatalf("spawn+wait failed: %s", out.Error)
	}
	if f.lastSpawn.AgentID != "researcher" {
		t.Errorf("must have spawned the target, got %+v", f.lastSpawn)
	}
	if len(out.Parts) == 0 || !strings.Contains(out.Parts[0].Text, "sub done") {
		t.Errorf("spawn+wait must return the finished snapshot content, got %+v", out.Parts)
	}
}

func TestAgentTool_RootResolvedFromSubSession(t *testing.T) {
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}
	// A sub-agent (session root1::agent::X) spawning a child must resolve the
	// SAME root table so the whole tree is one List.
	m.Dispatch(context.Background(), agentCall(
		map[string]any{"agent": "researcher", "task": "dig"}, "root1::agent::coding#abc", "coding#abc"))
	if f.lastSpawn.RootSession != "root1" {
		t.Errorf("nested spawn must resolve root1, got %q", f.lastSpawn.RootSession)
	}
	if f.lastSpawn.ParentRunID != "coding#abc" {
		t.Errorf("nested parent run id = %q, want coding#abc", f.lastSpawn.ParentRunID)
	}
}

func TestAgentTool_CoordinatorGated(t *testing.T) {
	m := &meta.MetaDispatcher{
		Agents:            &fakeAgents{},
		CoordinatorLookup: func(string, string) bool { return false },
	}
	out := m.Dispatch(context.Background(), agentCall(map[string]any{"agent": "x", "task": "y"}, "root", "main"))
	if out.Status != "errored" || !strings.Contains(out.Error, "coordinator") {
		t.Errorf("non-coordinator must be blocked, got %+v", out)
	}
}

func TestAgentTool_ListWaitCancel(t *testing.T) {
	f := &fakeAgents{listed: []meta.AgentSnapshot{{RunID: "a#1", Status: "running"}}}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}

	list := m.Dispatch(context.Background(), agentCall(map[string]any{"list": true}, "root", "main"))
	if list.Status != "completed" || !strings.Contains(list.Parts[0].Text, "a#1") {
		t.Errorf("list failed: %+v", list)
	}

	wait := m.Dispatch(context.Background(), agentCall(map[string]any{"wait": true, "run_id": "a#1"}, "root", "main"))
	if wait.Status != "completed" || !strings.Contains(wait.Parts[0].Text, "sub done") {
		t.Errorf("wait failed: %+v", wait)
	}

	cancel := m.Dispatch(context.Background(), agentCall(map[string]any{"cancel": true, "run_id": "a#1"}, "root", "main"))
	if cancel.Status != "completed" || f.cancelled != "a#1" {
		t.Errorf("cancel failed: %+v cancelled=%q", cancel, f.cancelled)
	}
}

// TestAgentTool_ExecuteToolAliasResolves : a real model (gpt-4o-mini, proven
// live) delegates by calling execute_tool with the BARE short name "agent"
// instead of the underscored FQN. The dispatcher must resolve the documented
// alias "agent" → agent_spawn.agent so the call routes to a spawn (and the gate
// sees module=agent_spawn). Regression for the live multi-agent failure.
func TestAgentTool_ExecuteToolAliasResolves(t *testing.T) {
	f := &fakeAgents{}
	m := &meta.MetaDispatcher{Agents: f, CoordinatorLookup: allowCoordinator}
	out := m.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name":   "agent",
			"params": map[string]any{"agent": "researcher", "task": "capital of France?", "wait": false},
		},
		AppID: "app", AgentID: "main", SessionID: "root1",
	})
	if out.Status != "completed" {
		t.Fatalf("execute_tool(name=\"agent\") must resolve to a spawn, got %+v", out)
	}
	if f.lastSpawn.AgentID != "researcher" {
		t.Errorf("alias 'agent' did not resolve to agent_spawn.agent spawn: %+v", f.lastSpawn)
	}
}

// TestAgentTool_NilCoordinatorLookupDeniesFailClosed locks the fail-closed
// posture : when no CoordinatorLookup is wired the role check can't be done, so
// the `agent` tool must be DENIED (never waved through). Regression for the
// audit-flagged fail-open.
func TestAgentTool_NilCoordinatorLookupDeniesFailClosed(t *testing.T) {
	m := &meta.MetaDispatcher{Agents: &fakeAgents{}} // CoordinatorLookup nil
	out := m.Dispatch(context.Background(), agentCall(map[string]any{"agent": "x", "task": "y"}, "root", "main"))
	if out.Status != "errored" || !strings.Contains(out.Error, "coordinator") {
		t.Errorf("nil CoordinatorLookup must fail closed (deny), got %+v", out)
	}
}

func TestAgentTool_NotWired(t *testing.T) {
	m := &meta.MetaDispatcher{} // no Agents
	out := m.Dispatch(context.Background(), agentCall(map[string]any{"agent": "x", "task": "y"}, "root", "main"))
	if out.Status != "errored" || !strings.Contains(out.Error, "not wired") {
		t.Errorf("expected not-wired error, got %+v", out)
	}
}
