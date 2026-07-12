package toolname

import "testing"

func TestResolveAlias_MangledInternalFQN(t *testing.T) {
	cases := map[string]string{
		"memory_set_goal":    "memory.set_goal",
		"memory__set_goal":   "memory.set_goal",
		"memory.set_goal":    "memory.set_goal",
		"set_goal":           "memory.set_goal",
		"memory_remember":    "memory.remember",
		"memory_task_update": "memory.task_update",
		"agent_spawn_agent":  "agent_spawn.agent",
		"filesystem_read":    "filesystem_read", // NOT internal → unchanged
	}
	for in, want := range cases {
		if got := ResolveAlias(Canonicalize(in)); got != want {
			t.Errorf("ResolveAlias(Canonicalize(%q)) = %q, want %q", in, got, want)
		}
	}
}
