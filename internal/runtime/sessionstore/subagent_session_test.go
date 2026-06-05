package sessionstore

import "testing"

func TestSubAgentSession(t *testing.T) {
	cases := []struct {
		name      string
		sid       string
		wantRoot  string
		wantRun   string
		wantIsSub bool
	}{
		{"plain session", "sess-123", "", "", false},
		{"single sub-agent", "root::agent::run-A", "root", "run-A", true},
		{"nested sub-agent", "root::agent::run-A::agent::run-B", "root", "run-B", true},
		{"root with colons", "app:u1:sess-9::agent::run-X", "app:u1:sess-9", "run-X", true},
		{"empty string", "", "", "", false},
		{"empty root rejected", "::agent::run-A", "", "", false},
		{"empty runID rejected", "root::agent::", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, run, isSub := SubAgentSession(c.sid)
			if isSub != c.wantIsSub || root != c.wantRoot || run != c.wantRun {
				t.Errorf("SubAgentSession(%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.sid, root, run, isSub, c.wantRoot, c.wantRun, c.wantIsSub)
			}
		})
	}
}
