// Package behavior is a doc-only placeholder — the behavior engine is NOT a bus
// module.
//
// It enforces per-agent behavioral rules (consecutive-tool limits, plan-stated
// gating, drift) using per-session counters/sets/flags held across the turn —
// stateful and turn-coupled, not a stateless bus module. It lives in :
//
//   - internal/runtime/behavior   — the engine (OnTurnStart / PreTool / PostTool / OnAgentText)
//
// wired per-app in the runtime engine and driven by the YAML `behavior_profile`.
package behavior
