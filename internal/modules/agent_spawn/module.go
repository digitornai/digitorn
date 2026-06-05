// Package agent_spawn is a doc-only placeholder — multi-agent delegation is NOT
// a bus module.
//
// The `agent` delegation tool spawns sub-agents that run their own isolated
// turns (goroutine + sub-session + live telemetry) — orchestration tightly
// coupled to the engine and session store, not a stateless bus module. It lives
// in :
//
//   - internal/runtime/agent                 — the orchestrator (Spawn / Wait / Cancel / telemetry)
//   - internal/runtime/context/meta/agent.go  — the `agent` meta-tool handler
//   - internal/runtime/subagent.go            — RunSubAgent (isolated sub-turn)
//
// Activation contract (docs-site/docs/reference/modules/agent_spawn.md +
// language/04b-builtin-tools.md — "gated by agent_spawn module loaded") : the
// module is loaded by declaring it under tools.modules.agent_spawn OR granting
// it in tools.capabilities.grant ({module: agent_spawn}). Only then is the
// single delegation tool — canonical FQN `agent_spawn.agent` (alias `Agent`) —
// injected (always-direct). A second coordinator-role gate then applies at
// dispatch time, so only coordinator-role agents may actually call it.
package agent_spawn
