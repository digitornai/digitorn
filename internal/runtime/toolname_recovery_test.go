package runtime

import (
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
)

// TestCanonicalize_BareActionRecovery : a weak model that drops the module
// prefix and calls "read" instead of "filesystem__read" must still resolve to
// filesystem.read — otherwise gate 1a denies it with an empty module ("access
// to module  …"). Recovery is from the offered toolset and only when the bare
// action is UNAMBIGUOUS.
func TestCanonicalize_BareActionRecovery(t *testing.T) {
	offered := []llm.ToolSpec{
		{Name: "filesystem__read"},
		{Name: "filesystem__glob"},
		{Name: "bash__run"},
		// Two modules expose "list" → ambiguous, must NOT be recovered.
		{Name: "alpha__list"},
		{Name: "beta__list"},
	}
	cases := []struct{ in, want string }{
		{"read", "filesystem.read"},          // bare domain action → recovered
		{"filesystem__read", "filesystem.read"}, // wire form → canonicalised
		{"filesystem_read", "filesystem.read"},  // SINGLE underscore (model collapsed __) → recovered
		{"filesystem.read", "filesystem.read"},  // already canonical → unchanged
		{"run", "bash.run"},                   // bare → recovered
		{"bash_run", "bash.run"},              // single underscore → recovered
		{"agent", "agent_spawn.agent"},        // internal alias still wins
		{"search_tools", "context_builder.search_tools"}, // meta-tool → qualified canonical
		{"list", "list"},                      // ambiguous : left bare (agent must qualify)
		{"nonexistent", "nonexistent"},        // unknown : left as-is
	}
	calls := make([]llm.ChatToolCall, len(cases))
	for i, c := range cases {
		calls[i] = llm.ChatToolCall{Name: c.in}
	}
	canonicalizeToolCallNames(calls, offered)
	for i, c := range cases {
		if got := calls[i].Name; got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
}

// MCP virtual tools ship a short wire alias ("echo") to the model; the inbound
// canonicalizer must map it back to the canonical "mcp_<server>.<tool>" via the
// spec's Canonical field, so the gates / dispatcher see the real FQN. The alias
// hit is authoritative and wins over the bare-action heuristics; native tools
// offered alongside still canonicalise normally.
func TestCanonicalize_MCPWireAlias(t *testing.T) {
	offered := []llm.ToolSpec{
		{Name: "filesystem__read"},
		{Name: "echo", Canonical: "mcp_everything.echo"},
		{Name: "sequentialthinking", Canonical: "mcp_sequential_thinking.sequentialthinking"},
	}
	cases := []struct{ in, want string }{
		{"echo", "mcp_everything.echo"},                                   // alias → canonical FQN
		{"sequentialthinking", "mcp_sequential_thinking.sequentialthinking"}, // alias with underscore server
		{"filesystem__read", "filesystem.read"},                           // native wire form still canonicalises
		{"read", "filesystem.read"},                                       // bare native recovered, not shadowed
	}
	calls := make([]llm.ChatToolCall, len(cases))
	for i, c := range cases {
		calls[i] = llm.ChatToolCall{Name: c.in}
	}
	canonicalizeToolCallNames(calls, offered)
	for i, c := range cases {
		if got := calls[i].Name; got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
}
