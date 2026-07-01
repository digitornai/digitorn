package toolname_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/toolname"
)

// =====================================================================
// Canonicalize — underscored wire form → dotted FQN
// =====================================================================

func TestCanonicalize_TableDriven(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// happy path
		{"filesystem__read", "filesystem.read"},
		{"shell__bash", "shell.bash"},
		{"context_builder__search_tools", "context_builder.search_tools"},

		// already canonical : no-op
		{"filesystem.read", "filesystem.read"},
		{"context_builder.execute_tool", "context_builder.execute_tool"},

		// no separator at all : bare action name
		{"search_tools", "search_tools"},
		{"run_parallel", "run_parallel"},

		// only the FIRST __ converts ; param-style suffixes preserved
		{"a__b__c", "a.b__c"},
		{"foo__bar__baz__quux", "foo.bar__baz__quux"},

		// :: separator (a form real models like deepseek emit) → dotted FQN.
		// Without this the module half is lost and gate1a denies the call.
		{"filesystem::glob", "filesystem.glob"},
		{"filesystem::grep", "filesystem.grep"},
		{"context_builder::search_tools", "context_builder.search_tools"},
		// only the FIRST :: converts ; suffixes preserved
		{"a::b::c", "a.b::c"},
		// a name already dotted is left untouched even if a :: follows
		{"filesystem.read::x", "filesystem.read::x"},

		// empty
		{"", ""},
	} {
		if got := toolname.Canonicalize(c.in); got != c.want {
			t.Errorf("Canonicalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalize_Idempotent(t *testing.T) {
	for _, in := range []string{
		"filesystem.read", "filesystem__read", "search_tools", "",
	} {
		once := toolname.Canonicalize(in)
		twice := toolname.Canonicalize(once)
		if once != twice {
			t.Errorf("Canonicalize not idempotent on %q : once=%q twice=%q",
				in, once, twice)
		}
	}
}

// =====================================================================
// Sanitize — canonical → underscored wire form
// =====================================================================

func TestSanitize_TableDriven(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"filesystem.read", "filesystem__read"},
		{"shell.bash", "shell__bash"},
		{"context_builder.search_tools", "context_builder__search_tools"},

		// already sanitized → idempotent
		{"filesystem__read", "filesystem__read"},

		// no dot
		{"search_tools", "search_tools"},

		// empty
		{"", ""},
	} {
		if got := toolname.Sanitize(c.in); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// =====================================================================
// Round-trips — the property the whole system depends on
// =====================================================================

func TestSanitize_Canonicalize_RoundTrip(t *testing.T) {
	// Every name the runtime stores in dotted form must survive the
	// wire-form round-trip the LLM imposes (Sanitize on outbound,
	// Canonicalize on inbound).
	for _, name := range []string{
		"filesystem.read",
		"filesystem.write",
		"shell.bash",
		"context_builder.search_tools",
		"context_builder.execute_tool",
		"http.get",
	} {
		round := toolname.Canonicalize(toolname.Sanitize(name))
		if round != name {
			t.Errorf("round-trip lost %q (intermediate=%q, final=%q)",
				name, toolname.Sanitize(name), round)
		}
	}
}

func TestCanonicalize_Sanitize_RoundTrip(t *testing.T) {
	// And the inverse : wire-form names also round-trip through
	// Canonicalize → Sanitize cleanly.
	for _, name := range []string{
		"filesystem__read", "shell__bash", "context_builder__execute_tool",
	} {
		round := toolname.Sanitize(toolname.Canonicalize(name))
		if round != name {
			t.Errorf("inverse round-trip lost %q (got %q)", name, round)
		}
	}
}

// =====================================================================
// SplitFQN
// =====================================================================

func TestSplitFQN(t *testing.T) {
	for _, c := range []struct {
		in               string
		wantMod, wantAct string
	}{
		{"filesystem.read", "filesystem", "read"},
		{"filesystem__read", "filesystem", "read"},
		{"context_builder.search_tools", "context_builder", "search_tools"},
		{"search_tools", "", "search_tools"}, // bare meta-tool
		{"", "", ""},
	} {
		mod, act := toolname.SplitFQN(c.in)
		if mod != c.wantMod || act != c.wantAct {
			t.Errorf("SplitFQN(%q) = (%q,%q), want (%q,%q)",
				c.in, mod, act, c.wantMod, c.wantAct)
		}
	}
}
