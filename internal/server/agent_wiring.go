package server

import (
	"context"
	"time"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/runtime/agent"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
)

// agentManagerAdapter bridges the concrete agent.Manager to the
// meta.AgentManager interface the `agent` delegation tool drives — converting
// the LLM's float seconds to a Duration and agent.Snapshot to meta.AgentSnapshot.
type agentManagerAdapter struct{ m *agent.Manager }

func (a agentManagerAdapter) Spawn(ctx context.Context, req meta.AgentSpawnRequest) (string, error) {
	return a.m.Spawn(ctx, agent.SpawnRequest{
		AppID:        req.AppID,
		RootSession:  req.RootSession,
		UserID:       req.UserID,
		UserJWT:      req.UserJWT,
		AgentID:      req.AgentID,
		Task:         req.Task,
		MemorySeed:   req.MemorySeed,
		ParentRunID:  req.ParentRunID,
		ParentCallID: req.ParentCallID,
	})
}

func (a agentManagerAdapter) Wait(ctx context.Context, root, runID string, secs float64) (meta.AgentSnapshot, error) {
	s, err := a.m.Wait(ctx, root, runID, secsToDur(secs))
	return toMetaSnap(s), err
}

func (a agentManagerAdapter) WaitAll(ctx context.Context, root string, ids []string, secs float64) ([]meta.AgentSnapshot, error) {
	snaps, err := a.m.WaitAll(ctx, root, ids, secsToDur(secs))
	out := make([]meta.AgentSnapshot, len(snaps))
	for i, s := range snaps {
		out[i] = toMetaSnap(s)
	}
	return out, err
}

func (a agentManagerAdapter) Status(root, runID string) (meta.AgentSnapshot, error) {
	s, err := a.m.Status(root, runID)
	return toMetaSnap(s), err
}

func (a agentManagerAdapter) List(root string) []meta.AgentSnapshot {
	snaps := a.m.List(root)
	out := make([]meta.AgentSnapshot, len(snaps))
	for i, s := range snaps {
		out[i] = toMetaSnap(s)
	}
	return out
}

func (a agentManagerAdapter) SpawnBatch(ctx context.Context, reqs []meta.AgentSpawnRequest) ([]string, error) {
	batch := make([]agent.SpawnRequest, len(reqs))
	for i, r := range reqs {
		batch[i] = agent.SpawnRequest{
			AppID: r.AppID, RootSession: r.RootSession, UserID: r.UserID,
			UserJWT: r.UserJWT, AgentID: r.AgentID, Task: r.Task,
			MemorySeed: r.MemorySeed, ParentRunID: r.ParentRunID, ParentCallID: r.ParentCallID,
		}
	}
	return a.m.SpawnBatch(ctx, batch)
}

func (a agentManagerAdapter) Cancel(root, runID string) error { return a.m.Cancel(root, runID) }

func secsToDur(s float64) time.Duration {
	if s <= 0 {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

func toMetaSnap(s agent.Snapshot) meta.AgentSnapshot {
	return meta.AgentSnapshot{
		RunID: s.RunID, AgentID: s.AgentID, ParentRunID: s.ParentRunID, Status: s.Status,
		Depth: s.Depth, DurationMs: s.DurationMs,
		ToolCalls: s.ToolCalls, LLMCalls: s.LLMCalls, TokensIn: s.TokensIn, TokensOut: s.TokensOut,
		Children: s.Children, Content: s.Content, Error: s.Error,
	}
}

// newCoordinatorLookup gates the `agent` tool : only an agent whose role is
// "coordinator" may delegate. Reads the lock-free app snapshot.
func newCoordinatorLookup(apps appmgr.Manager) func(appID, agentID string) bool {
	return func(appID, agentID string) bool {
		ra, err := apps.Get(context.Background(), appID)
		if err != nil || ra == nil || ra.Definition == nil {
			return false
		}
		for i := range ra.Definition.Agents {
			if ra.Definition.Agents[i].ID == agentID {
				return ra.Definition.Agents[i].Role == "coordinator"
			}
		}
		return false
	}
}

var _ meta.AgentManager = agentManagerAdapter{}
