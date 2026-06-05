// Package meta implements the context_builder's runtime surface :
// the 5 documented meta-tools (search_tools, get_tool, execute_tool,
// list_categories, browse_category), the auto-routing that lets the
// LLM call domain tools by their short FQN, and the OpenAI-compat
// tool-name sanitization.
//
// Documented in :
//   - docs-site/docs/language/04-tools.md "Tool name sanitization"
//   - docs-site/docs/language/04-tools.md "Auto-routing direct calls"
//   - docs-site/docs/language/04b-builtin-tools.md
//
// CB-3 (this package) wires the meta-tools to real implementations
// that read from the CB-1 ToolIndex. The auto-routing is transparent :
// a tool_call with name="filesystem.read" just falls through to the
// inner dispatcher (D1 module dispatcher in production) ; the meta
// layer only intercepts names that start with "context_builder.".
package meta

import (
	"strings"

	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
)

// Canonicalize, Sanitize and SplitFQN are kept here as thin
// pass-throughs so the existing meta-package consumers (and their
// tests) keep working unchanged. The canonical implementation lives
// in internal/runtime/toolname — a leaf package with no internal
// imports — so the runtime engine can import it too without an
// import cycle.
func Canonicalize(name string) string { return toolname.Canonicalize(name) }
func Sanitize(name string) string     { return toolname.Sanitize(name) }

// ResolveAlias maps a documented short alias for a runtime-internal tool
// (Agent / Remember / TaskCreate / …) to its canonical FQN. Thin pass-through
// to toolname so the dispatcher resolves the bare names real models emit.
func ResolveAlias(name string) string { return toolname.ResolveAlias(name) }
func SplitFQN(name string) (module, action string) {
	return toolname.SplitFQN(name)
}

// IsContextBuilderMeta reports whether the canonical name is a
// context_builder meta-tool or always-direct primitive that this
// dispatcher intercepts. Per docs-site/docs/language/04b-builtin-tools.md
// "Always-available primitives (context_builder)" : the 5 discovery
// meta-tools + the 5 always-direct primitives. The `agent` delegation tool
// (agent_spawn module) and the memory tools (memory module) are NOT here —
// they're module-gated and routed via IsAgentSpawnTool / IsMemoryTool.
func IsContextBuilderMeta(canonicalName string) bool {
	const prefix = "context_builder."
	if !strings.HasPrefix(canonicalName, prefix) {
		return false
	}
	switch canonicalName[len(prefix):] {
	case "search_tools",
		"get_tool",
		"execute_tool",
		"list_categories",
		"browse_category",
		// P-1 always-direct primitives :
		"run_parallel",
		"background_run",
		"use_skill",
		"call_app",
		"ask_user":
		return true
	}
	return false
}

// IsMemoryTool reports whether the canonical name is one of the `memory`
// module's 4 LLM-exposed actions (docs-site/docs/reference/modules/memory.md).
// They're gated by the app declaring tools.modules.memory but, once active,
// the dispatcher intercepts them (memory is a runtime subsystem, not a bus
// module) and routes to the MemoryWriter — they never reach the inner
// dispatcher.
func IsMemoryTool(canonicalName string) bool {
	const prefix = "memory."
	if !strings.HasPrefix(canonicalName, prefix) {
		return false
	}
	switch canonicalName[len(prefix):] {
	case "set_goal", "remember", "task_create", "task_update":
		return true
	}
	return false
}

// IsAgentSpawnTool reports whether the canonical name is the `agent_spawn`
// module's single delegation action (docs-site/docs/reference/modules/
// agent_spawn.md — tool name `Agent`). Gated by the agent_spawn module being
// loaded ; the dispatcher intercepts it and routes to the AgentManager.
func IsAgentSpawnTool(canonicalName string) bool {
	return canonicalName == "agent_spawn.agent"
}
