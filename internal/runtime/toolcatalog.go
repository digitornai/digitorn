package runtime

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
)

// ToolCatalog is what the runtime depends on to translate an agent's
// declared module references into the [] LLM-facing ToolSpec the model
// expects. The actual resolution (module → list of tool defs with
// JSON-schema params) lives outside the runtime — in the daemon's
// module registry — so this package stays insulated from where tools
// physically live.
//
// V1 production implementation : *DaemonCatalog (wired at boot from the
// module registry). V1 fallback when no registry is available :
// NoToolsCatalog (returns nil, agents run without tools).
// Tests : a stub that returns canned ToolSpec entries.
type ToolCatalog interface {
	// ToolsForAgent returns the list of tools the agent is permitted
	// to call. The order is significant — providers display tools in
	// the order received, and some models bias selection accordingly.
	// Return nil (NOT a zero-length slice) when the agent has no tools.
	ToolsForAgent(agent *schema.Agent) []llm.ToolSpec
}

// NoToolsCatalog is the trivial implementation that says "this agent
// has no tools". Used as the default when the daemon hasn't wired a
// real catalog, so the runtime never panics on a nil Catalog and the
// test bench keeps working.
type NoToolsCatalog struct{}

// ToolsForAgent always returns nil — V0 path, equivalent to single-turn
// chat with no tool calls.
func (NoToolsCatalog) ToolsForAgent(_ *schema.Agent) []llm.ToolSpec {
	return nil
}

// StaticToolCatalog is a test helper : configure a fixed list of tools
// at construction, return them for every agent. Lets unit tests assert
// that the runtime wires ToolsForAgent into ChatRequest.Tools without
// pulling in the full module registry.
type StaticToolCatalog struct {
	Tools []llm.ToolSpec
}

func (c *StaticToolCatalog) ToolsForAgent(_ *schema.Agent) []llm.ToolSpec {
	if len(c.Tools) == 0 {
		return nil
	}
	out := make([]llm.ToolSpec, len(c.Tools))
	copy(out, c.Tools)
	return out
}
