package toolname

import "testing"

// TestResolveMangled covers the model-mangled tool-name recovery: real models
// freely swap "." / "_" / "__" in tool names — especially MCP names of the form
// mcp_<server>.<tool> — which Canonicalize alone can't undo (it can't know the
// module boundary). ResolveMangled recovers them against the known FQN set.
func TestResolveMangled(t *testing.T) {
	fqns := []string{
		"mcp_notion.notion-search",
		"mcp_notion.notion-create-pages",
		"filesystem.read",
		"a.b_c", // collides (flattened) with a_b.c below
		"a_b.c",
	}
	cases := map[string]string{
		"mcp_notion_notion-search":        "mcp_notion.notion-search",       // single-underscore mangle of an MCP FQN
		"mcp_notion__notion-search":       "mcp_notion.notion-search",       // sanitized double-underscore wire form
		"mcp_notion_notion-create-pages":  "mcp_notion.notion-create-pages", // hyphenated action survives
		"filesystem_read":                 "filesystem.read",                // native single-underscore
		"a_b_c":                           "a_b_c",                          // ambiguous (two FQNs flatten to it) → unchanged
		"mcp_notion.notion-search":        "mcp_notion.notion-search",       // already canonical → untouched
		"totally_unknown_tool":            "totally_unknown_tool",           // no match → unchanged
		"":                                "",
	}
	for in, want := range cases {
		if got := ResolveMangled(in, fqns); got != want {
			t.Errorf("ResolveMangled(%q) = %q, want %q", in, got, want)
		}
	}
}
