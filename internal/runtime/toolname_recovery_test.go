package runtime

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
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
		{"search_tools", "search_tools"},      // meta-tool : no module, left bare
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
