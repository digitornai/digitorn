package meta

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitornai/digitorn/internal/runtime"
)

type AgentManager interface {
	Spawn(ctx context.Context, req AgentSpawnRequest) (runID string, err error)
	SpawnBatch(ctx context.Context, reqs []AgentSpawnRequest) ([]string, error)
	Wait(ctx context.Context, rootSession, runID string, timeoutSecs float64) (AgentSnapshot, error)
	WaitAll(ctx context.Context, rootSession string, runIDs []string, timeoutSecs float64) ([]AgentSnapshot, error)
	Status(rootSession, runID string) (AgentSnapshot, error)
	List(rootSession string) []AgentSnapshot
	Cancel(rootSession, runID string) error
	CancelTree(rootSession, runID string) int
}

type AgentKVStore interface {
	Set(root, key, value string)
	Get(root, key string) (string, bool)
	Delete(root, key string)
	All(root string) map[string]string
}

type AgentSpawnRequest struct {
	AppID        string
	RootSession  string
	UserID       string
	UserJWT      string
	AgentID      string
	Task         string
	MemorySeed   string
	ParentRunID  string
	ParentCallID string

	// InheritContext = fork mode (seed with the parent transcript). Default
	// false = current isolated sub-agent behavior.
	InheritContext bool
}

type AgentSnapshot struct {
	RunID       string `json:"run_id"`
	AgentID     string `json:"agent_id"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	Status      string `json:"status"`
	Depth       int    `json:"depth"`
	DurationMs  int64  `json:"duration_ms"`
	ToolCalls   int64  `json:"tool_calls"`
	LLMCalls    int64  `json:"llm_calls"`
	TokensIn    int64  `json:"tokens_in"`
	TokensOut   int64  `json:"tokens_out"`
	Children    int64  `json:"children"`
	Content     string `json:"content,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (m *MetaDispatcher) handleAgent(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.Agents == nil {
		return errored("agent not wired (no AgentManager)")
	}
	if m.CoordinatorLookup == nil || !m.CoordinatorLookup(call.AppID, call.AgentID) {
		return errored("the `agent` tool requires a coordinator-role agent")
	}
	root := rootSessionOf(m.SessionID(call))

	if list, _ := call.Args["list"].(bool); list {
		return jsonOutcome(map[string]any{"agents": m.Agents.List(root)})
	}
	if cancel, _ := call.Args["cancel"].(bool); cancel {
		runID, _ := call.Args["run_id"].(string)
		if runID == "" {
			return errored("agent cancel: 'run_id' is required")
		}
		tree, _ := call.Args["cancel_tree"].(bool)
		if tree {
			n := m.Agents.CancelTree(root, runID)
			return jsonOutcome(map[string]any{"cancelled": runID, "cancelled_count": n})
		}
		if err := m.Agents.Cancel(root, runID); err != nil {
			return errored("agent cancel: " + err.Error())
		}
		return jsonOutcome(map[string]any{"cancelled": runID})
	}

	wait, _ := call.Args["wait"].(bool)
	timeout, _ := call.Args["timeout"].(float64)

	if raw, ok := call.Args["agents"]; ok {
		items, _ := raw.([]any)
		if len(items) == 0 {
			return errored("agent batch: 'agents' must be a non-empty array of {agent, task}")
		}
		reqs := make([]AgentSpawnRequest, 0, len(items))
		for i, item := range items {
			m, _ := item.(map[string]any)
			if m == nil {
				return errored(fmt.Sprintf("agent batch: item %d is not an object", i))
			}
			target := firstString(m, "agent", "specialist")
			task := firstString(m, "task", "prompt")
			if target == "" || task == "" {
				return errored(fmt.Sprintf("agent batch: item %d must have 'agent' and 'task'", i))
			}
			seed, _ := m["memory_seed"].(string)
			ftyp, _ := m["type"].(string)
			finh, _ := m["inherit_context"].(bool)
			reqs = append(reqs, AgentSpawnRequest{
				AppID: call.AppID, RootSession: root, UserID: call.UserID,
				UserJWT: call.UserJWT, AgentID: target, Task: task,
				MemorySeed: seed, ParentRunID: call.AgentRunID, ParentCallID: call.CallID,
				InheritContext: ftyp == "fork" || finh,
			})
		}
		runIDs, err := m.Agents.SpawnBatch(ctx, reqs)
		if err != nil {
			return errored("agent batch: " + err.Error())
		}
		if !wait {
			return jsonOutcome(map[string]any{"run_ids": runIDs, "count": len(runIDs), "status": "running"})
		}
		snaps, werr := m.Agents.WaitAll(ctx, root, runIDs, timeout)
		if werr != nil {
			return errored("agent batch wait: " + werr.Error())
		}
		return jsonOutcome(map[string]any{"agents": snaps})
	}

	if target := firstString(call.Args, "agent", "specialist"); target != "" {
		task := firstString(call.Args, "task", "prompt")
		if task == "" {
			return errored("agent spawn: 'task' is required")
		}
		seed, _ := call.Args["memory_seed"].(string)
		typ, _ := call.Args["type"].(string)
		inh, _ := call.Args["inherit_context"].(bool)
		runID, err := m.Agents.Spawn(ctx, AgentSpawnRequest{
			AppID:          call.AppID,
			RootSession:    root,
			UserID:         call.UserID,
			UserJWT:        call.UserJWT,
			AgentID:        target,
			Task:           task,
			MemorySeed:     seed,
			ParentRunID:    call.AgentRunID,
			ParentCallID:   call.CallID,
			InheritContext: typ == "fork" || inh,
		})
		if err != nil {
			return errored("agent spawn: " + err.Error())
		}
		if !wait {
			return jsonOutcome(map[string]any{"run_id": runID, "status": "running"})
		}
		snap, werr := m.Agents.Wait(ctx, root, runID, timeout)
		if werr != nil {
			return errored("agent wait: " + werr.Error())
		}
		return jsonOutcome(agentSnapMap(snap))
	}

	if wait {
		if ids := stringSliceArg(call.Args["run_ids"]); len(ids) > 0 {
			snaps, err := m.Agents.WaitAll(ctx, root, ids, timeout)
			if err != nil {
				return errored("agent wait_all: " + err.Error())
			}
			return jsonOutcome(map[string]any{"agents": snaps})
		}
		runID, _ := call.Args["run_id"].(string)
		if runID == "" {
			return errored("agent wait: 'run_id' or 'run_ids' is required")
		}
		snap, err := m.Agents.Wait(ctx, root, runID, timeout)
		if err != nil {
			return errored("agent wait: " + err.Error())
		}
		return jsonOutcome(agentSnapMap(snap))
	}
	if runID, _ := call.Args["run_id"].(string); runID != "" {
		snap, err := m.Agents.Status(root, runID)
		if err != nil {
			return errored("agent status: " + err.Error())
		}
		result := agentSnapMap(snap)
		if snap.Status == "running" {
			result["_hint"] = "Sub-agent still running. Stop polling — call agent(run_id=\"" + runID + "\", wait=true) to block until it finishes instead of checking status repeatedly."
		}
		return jsonOutcome(result)
	}
	return errored("agent spawn: 'agent' (target agent id) is required")
}

func rootSessionOf(s string) string {
	if i := strings.Index(s, "::agent::"); i >= 0 {
		return s[:i]
	}
	return s
}

func firstString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func agentSnapMap(s AgentSnapshot) map[string]any {
	m := map[string]any{
		"run_id": s.RunID, "agent_id": s.AgentID, "status": s.Status,
		"depth": s.Depth, "duration_ms": s.DurationMs,
		"tool_calls": s.ToolCalls, "llm_calls": s.LLMCalls,
		"tokens_in": s.TokensIn, "tokens_out": s.TokensOut, "children": s.Children,
	}
	if s.ParentRunID != "" {
		m["parent_run_id"] = s.ParentRunID
	}
	if s.Content != "" {
		m["content"] = s.Content
	}
	if s.Error != "" {
		m["error"] = s.Error
	}
	return m
}

func (m *MetaDispatcher) handleKV(call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.KV == nil {
		return errored("kv not wired")
	}
	root := rootSessionOf(m.SessionID(call))
	key, _ := call.Args["key"].(string)

	if del, _ := call.Args["delete"].(bool); del {
		if key == "" {
			return errored("kv delete: 'key' is required")
		}
		m.KV.Delete(root, key)
		return jsonOutcome(map[string]any{"deleted": key})
	}
	if list, _ := call.Args["list"].(bool); list {
		return jsonOutcome(map[string]any{"entries": m.KV.All(root)})
	}
	if value, ok := call.Args["value"].(string); ok {
		if key == "" {
			return errored("kv set: 'key' is required")
		}
		m.KV.Set(root, key, value)
		return jsonOutcome(map[string]any{"key": key, "written": true})
	}
	if key == "" {
		return errored("kv: 'key' is required for read/write")
	}
	v, found := m.KV.Get(root, key)
	if !found {
		return jsonOutcome(map[string]any{"key": key, "found": false})
	}
	return jsonOutcome(map[string]any{"key": key, "value": v, "found": true})
}
