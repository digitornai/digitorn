//go:build live

package runtime_test

import (
	"strings"
	"testing"
)

// TestLiveMemory_AgentSetsGoalAndRemembers : the memory tools proven against the
// REAL gateway. A real LLM is told to use set_goal + remember ; we assert the
// mutations landed durably in session state (event-sourced — a single path, no
// side store). This is the end-to-end proof that the tools are offered, the
// model calls them, and they project correctly.
func TestLiveMemory_AgentSetsGoalAndRemembers(t *testing.T) {
	f := liveSetup(t)

	f.runLive(t, "Use your memory tools now. First call set_goal with goal "+
		"'investigate the authentication bug'. Then call remember with content "+
		"'the test command is: go test ./auth/ -run TestVerify'. Then confirm in one sentence what you stored.")

	st, _ := f.session.State("live-sess")
	snap := st.Snapshot()

	if !strings.Contains(strings.ToLower(snap.Goal), "auth") {
		t.Errorf("goal not set via the real LLM tool call, got %q", snap.Goal)
	}
	var sawFact bool
	for _, fct := range snap.Facts {
		if strings.Contains(strings.ToLower(fct), "go test") {
			sawFact = true
		}
	}
	if !sawFact {
		t.Errorf("fact not remembered via the real LLM, facts=%v", snap.Facts)
	}
	t.Logf("durable memory after live turn : goal=%q facts=%v", snap.Goal, snap.Facts)
}
