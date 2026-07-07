package catalog_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/catalog"
)


func TestSystemModulesSeeded(t *testing.T) {
	cat := catalog.Empty() // no external sources — only the built-in seed

	cases := []struct {
		module string
		tools  []string
	}{
		{"memory", []string{"set_goal", "remember", "task_create", "task_update"}},
		{"agent_spawn", []string{"agent"}},

		{"channels", nil},
	}
	for _, c := range cases {
		if !cat.HasModule(c.module) {
			t.Errorf("catalog must know system module %q (apps can't declare it otherwise)", c.module)
			continue
		}
		for _, tool := range c.tools {
			if !cat.HasTool(c.module, tool) {
				t.Errorf("catalog: module %q missing tool %q (grants on it would fail to compile)", c.module, tool)
			}
		}
	}
}
