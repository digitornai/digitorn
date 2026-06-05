package dispatch

import "testing"

// TestAliasLegacyToolModule proves the dispatch-time safety net routes legacy
// workspace FILE ops to filesystem while leaving the new workspace module's git
// tools (and every other module) untouched.
func TestAliasLegacyToolModule(t *testing.T) {
	cases := []struct{ mod, act, want string }{
		// legacy file ops → filesystem
		{"workspace", "read", "filesystem"},
		{"workspace", "write", "filesystem"},
		{"workspace", "edit", "filesystem"},
		{"workspace", "multi_edit", "filesystem"},
		{"workspace", "glob", "filesystem"},
		{"workspace", "grep", "filesystem"},
		{"workspace", "delete", "filesystem"},
		// new workspace git tools stay on workspace
		{"workspace", "baseline", "workspace"},
		{"workspace", "changes", "workspace"},
		{"workspace", "diff", "workspace"},
		{"workspace", "commit", "workspace"},
		// unrelated modules untouched
		{"filesystem", "write", "filesystem"},
		{"shell", "exec", "shell"},
		{"memory", "remember", "memory"},
	}
	for _, c := range cases {
		if got := aliasLegacyToolModule(c.mod, c.act); got != c.want {
			t.Errorf("aliasLegacyToolModule(%q,%q)=%q want %q", c.mod, c.act, got, c.want)
		}
	}
}
