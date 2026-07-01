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

// AgentSpawnRequest is one delegation issued by a coordinator.
type AgentSpawnRequest struct {
	AppID        string
	RootSession  string
	UserID       string
	UserJWT      string // gateway bearer forwarded to the sub-agent's isolated turn
	AgentID      string // target logical agent id
	Task         string
	MemorySeed   string
	ParentRunID  string // the calling agent's run id ("" / logical for the entry agent)
	ParentCallID string // the tool call_id of the delegating `agent` call (chip key)
}

// AgentSnapshot is the live view of one agent returned to the LLM.
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

// handleAgent dispatches the `agent` delegation tool. Modes (mirroring the
// reference agent_spawn) :
//
//	spawn          : { agent, task, memory_seed? }        → { run_id, status }
//	spawn + wait   : { agent, task, wait:true, timeout? } → finished snapshot
//	wait           : { wait:true, run_id|run_ids, timeout? }
//	status         : { run_id }
//	list           : { list:true }                        → { agents:[...] }
//	cancel         : { cancel:true, run_id }
//
// The presence of a delegation target (`agent` / `specialist`) selects spawn —
// a target with wait:true spawns THEN blocks on the child and returns its
// finished snapshot, so a coordinator delegates and collects the answer in a
// single tool call. Without a target, wait/status operate on existing run ids.
//
// Only coordinator-role agents may call it.
func (m *MetaDispatcher) handleAgent(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.Agents == nil {
		return errored("agent not wired (no AgentManager)")
	}
	// Fail CLOSED : the `agent` tool is reserved for coordinator-role agents.
	// A nil lookup means the role check can't be performed, so we DENY rather
	// than wave the call through — a missing dependency must never silently
	// disable a security gate (the same fail-open class as the gate2 risk
	// ceiling). Production always wires CoordinatorLookup ; the tool is also
	// gated upstream by the app declaring tools.modules.agent_spawn.
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

	// Batch spawn: agents=[{agent:"x",task:"..."}, ...] launches N agents atomically.
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
			reqs = append(reqs, AgentSpawnRequest{
				AppID: call.AppID, RootSession: root, UserID: call.UserID,
				UserJWT: call.UserJWT, AgentID: target, Task: task,
				MemorySeed: seed, ParentRunID: call.AgentRunID, ParentCallID: call.CallID,
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

	// A delegation target selects spawn — optionally with an inline wait.
	if target := firstString(call.Args, "agent", "specialist"); target != "" {
		task := firstString(call.Args, "task", "prompt")
		if task == "" {
			return errored("agent spawn: 'task' is required")
		}
		seed, _ := call.Args["memory_seed"].(string)
		runID, err := m.Agents.Spawn(ctx, AgentSpawnRequest{
			AppID:        call.AppID,
			RootSession:  root,
			UserID:       call.UserID,
			UserJWT:      call.UserJWT,
			AgentID:      target,
			Task:         task,
			MemorySeed:   seed,
			ParentRunID:  call.AgentRunID,
			ParentCallID: call.CallID,
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

	// No target : control operations on already-running agents.
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

// rootSessionOf strips the "::agent::<id>" sub-session suffix(es) so a
// sub-agent calling the tool resolves the SAME root table as the entry agent.
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
