// Package context_builder is a doc-only placeholder — the context builder is
// NOT a bus module.
//
// It assembles the system prompt and picks the agent's tool list ON EVERY TURN,
// so it is the turn's "central nervous system", deeply coupled to the engine,
// the session and the tool index — not a stateless bus module. It lives in :
//
//   - internal/runtime/context/index      — the per-agent ToolIndex (CB-1)
//   - internal/runtime/context/injection  — adaptive tool-injection planner (CB-2)
//   - internal/runtime/context/meta        — the meta-tools + auto-routing (CB-3)
//   - internal/runtime/context/prompt      — the 9-section system-prompt assembler (CB-4)
//   - internal/runtime/context/wiring      — the runtime.ContextBuilder glue (CB-6)
//
// Enabled the documented way :  modules: { context_builder: {} }
package context_builder
