package runtime

import "testing"

// splitToolName feeds the security gate (module/action) and the workdir
// enforcer. A model may emit the wire (`__`) or `::` separator instead of the
// canonical dot ; if the split is not canonicalisation-aware the module half is
// lost (module="") and gate1a denies every such call. This locks the
// chokepoint against that regression for all the forms a model can produce.
func TestSplitToolName_SeparatorForms(t *testing.T) {
	for _, c := range []struct{ in, module, action string }{
		{"filesystem.glob", "filesystem", "glob"},
		{"filesystem__glob", "filesystem", "glob"},
		{"filesystem::glob", "filesystem", "glob"},
		{"filesystem::grep", "filesystem", "grep"},
		{"context_builder::search_tools", "context_builder", "search_tools"},
		// bare meta-tool name : no module, action is the whole name
		{"search_tools", "", "search_tools"},
		{"", "", ""},
	} {
		module, action := splitToolName(c.in)
		if module != c.module || action != c.action {
			t.Errorf("splitToolName(%q) = (%q, %q), want (%q, %q)",
				c.in, module, action, c.module, c.action)
		}
	}
}
