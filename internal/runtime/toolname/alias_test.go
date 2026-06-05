package toolname_test

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
)

// TestResolveAlias locks in the documented short-name aliases for the
// runtime-internal tools (04b-builtin-tools.md). Real models emit these bare
// names instead of the underscored FQN — resolving them is what makes live
// delegation + memory work.
func TestResolveAlias(t *testing.T) {
	cases := map[string]string{
		// internal-tool aliases → canonical FQN
		"agent":       "agent_spawn.agent",
		"Agent":       "agent_spawn.agent",
		"remember":    "memory.remember",
		"Remember":    "memory.remember",
		"set_goal":    "memory.set_goal",
		"SetGoal":     "memory.set_goal",
		"task_create": "memory.task_create",
		"TaskCreate":  "memory.task_create",
		"task_update": "memory.task_update",
		"TaskUpdate":  "memory.task_update",
		// already-qualified / unknown names pass through untouched
		"memory.remember":              "memory.remember",
		"agent_spawn.agent":            "agent_spawn.agent",
		"filesystem.read":              "filesystem.read",
		"context_builder.execute_tool": "context_builder.execute_tool",
		"search_tools":                 "search_tools",
		"":                             "",
	}
	for in, want := range cases {
		if got := toolname.ResolveAlias(in); got != want {
			t.Errorf("ResolveAlias(%q) = %q, want %q", in, got, want)
		}
	}
}
