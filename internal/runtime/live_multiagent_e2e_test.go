//go:build live

package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/agent"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
)

// liveAgentAdapter bridges agent.Manager → meta.AgentManager for the live
// fixture (seconds→Duration, snapshot conversion).
type liveAgentAdapter struct{ m *agent.Manager }

func (a liveAgentAdapter) Spawn(ctx context.Context, req meta.AgentSpawnRequest) (string, error) {
	return a.m.Spawn(ctx, liveSpawnReq(req))
}
func (a liveAgentAdapter) SpawnBatch(ctx context.Context, reqs []meta.AgentSpawnRequest) ([]string, error) {
	out := make([]agent.SpawnRequest, len(reqs))
	for i, r := range reqs {
		out[i] = liveSpawnReq(r)
	}
	return a.m.SpawnBatch(ctx, out)
}
func liveSpawnReq(req meta.AgentSpawnRequest) agent.SpawnRequest {
	return agent.SpawnRequest{
		AppID: req.AppID, RootSession: req.RootSession, UserID: req.UserID, UserJWT: req.UserJWT,
		AgentID: req.AgentID, Task: req.Task, MemorySeed: req.MemorySeed, ParentRunID: req.ParentRunID,
	}
}
func (a liveAgentAdapter) Wait(ctx context.Context, root, runID string, secs float64) (meta.AgentSnapshot, error) {
	s, err := a.m.Wait(ctx, root, runID, time.Duration(secs*float64(time.Second)))
	return liveToMetaSnap(s), err
}
func (a liveAgentAdapter) WaitAll(ctx context.Context, root string, ids []string, secs float64) ([]meta.AgentSnapshot, error) {
	snaps, err := a.m.WaitAll(ctx, root, ids, time.Duration(secs*float64(time.Second)))
	out := make([]meta.AgentSnapshot, len(snaps))
	for i, s := range snaps {
		out[i] = liveToMetaSnap(s)
	}
	return out, err
}
func (a liveAgentAdapter) Status(root, runID string) (meta.AgentSnapshot, error) {
	s, err := a.m.Status(root, runID)
	return liveToMetaSnap(s), err
}
func (a liveAgentAdapter) List(root string) []meta.AgentSnapshot {
	snaps := a.m.List(root)
	out := make([]meta.AgentSnapshot, len(snaps))
	for i, s := range snaps {
		out[i] = liveToMetaSnap(s)
	}
	return out
}
func (a liveAgentAdapter) Cancel(root, runID string) error { return a.m.Cancel(root, runID) }

func (a liveAgentAdapter) CancelTree(root, runID string) int { return a.m.CancelTree(root, runID) }

func liveToMetaSnap(s agent.Snapshot) meta.AgentSnapshot {
	return meta.AgentSnapshot{
		RunID: s.RunID, AgentID: s.AgentID, ParentRunID: s.ParentRunID, Status: s.Status,
		Depth: s.Depth, DurationMs: s.DurationMs, ToolCalls: s.ToolCalls, LLMCalls: s.LLMCalls,
		TokensIn: s.TokensIn, TokensOut: s.TokensOut, Children: s.Children, Content: s.Content, Error: s.Error,
	}
}

// TestLiveMultiAgent_CoordinatorDelegatesToSpecialist : a REAL LLM coordinator
// calls the `agent` tool to delegate to a specialist sub-agent, which runs its
// own isolated turn and answers ; the coordinator collects the result and
// reports it. End-to-end proof through the live gateway.
func TestLiveMultiAgent_CoordinatorDelegatesToSpecialist(t *testing.T) {
	f := liveSetup(t)

	// Entry agent becomes a coordinator ; add a research specialist sharing the
	// same brain.
	f.app.Definition.Agents[0].Role = "coordinator"
	f.app.Definition.Agents = append(f.app.Definition.Agents, schema.Agent{
		ID:           "researcher",
		Role:         "specialist",
		Brain:        f.app.Definition.Agents[0].Brain,
		SystemPrompt: "You are a research specialist. Answer the delegated task with a single clear factual sentence.",
	})

	f.runLive(t, "You are a coordinator. Use the `agent` tool to delegate to the 'researcher' specialist "+
		"(agent=\"researcher\", task=\"What is the capital of France?\", wait=true), then report the specialist's answer.")

	// The strongest proof : a researcher sub-agent actually ran in the tree.
	var found bool
	for _, s := range f.agents.List("live-sess") {
		if s.AgentID == "researcher" && (s.Status == "completed" || s.Status == "errored") {
			found = true
			t.Logf("sub-agent %s status=%s tokens_in=%d tokens_out=%d content=%q",
				s.RunID, s.Status, s.TokensIn, s.TokensOut, s.Content)
		}
	}
	if !found {
		t.Fatalf("expected a researcher sub-agent in the tree, got %+v", f.agents.List("live-sess"))
	}

	// RESYNC : the durable agent tree projected into the root session's state —
	// what a reconnecting client reconstructs from the event log, with no live
	// registry. Proves the durable path fires under the real engine, not just
	// the in-memory registry.
	st, _ := f.session.State("live-sess")
	var durableFound bool
	for _, c := range st.Snapshot().Children {
		if c.Kind == "researcher" {
			durableFound = true
			t.Logf("durable child : run_id=%s status=%s tokens_in=%d tokens_out=%d",
				c.RunID, c.Status, c.TokensIn, c.TokensOut)
		}
	}
	if !durableFound {
		t.Fatalf("expected a durable researcher child in state.Children, got %+v", st.Snapshot().Children)
	}

	// The delegated answer (Paris) flowed back to the coordinator's reply.
	assertSemantic(t, f, "Paris")
}
