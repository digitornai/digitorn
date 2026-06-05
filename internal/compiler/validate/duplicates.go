package validate

import (
	"fmt"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
)

func (v *validator) checkDuplicates() {
	seen := map[string]int{}
	for i, a := range v.def.Agents {
		if a.ID == "" {
			continue
		}
		if first, dup := seen[a.ID]; dup {
			v.errf(diagnostic.CodeDuplicateID,
				fmt.Sprintf("agents.%d.id", i),
				"duplicate agent id %q (first declared at agents[%d])", a.ID, first)
			continue
		}
		seen[a.ID] = i
	}

	if rt := v.def.Runtime; rt != nil {
		v.checkUniqueIDs("runtime.triggers", "id", len(rt.Triggers), func(i int) string {
			return rt.Triggers[i].ID
		})
		v.checkUniqueIDs("runtime.hooks", "id", len(rt.Hooks), func(i int) string {
			return rt.Hooks[i].ID
		})
	}

	for i, a := range v.def.Agents {
		base := fmt.Sprintf("agents.%d.hooks", i)
		v.checkUniqueIDs(base, "id", len(a.Hooks), func(j int) string {
			return a.Hooks[j].ID
		})
	}

	if d := v.def.Dev; d != nil {
		v.checkUniqueIDs("dev.skills", "command", len(d.Skills), func(i int) string {
			return d.Skills[i].Command
		})
	}

	if v.def.Flow != nil {
		v.checkUniqueIDs("flow.nodes", "id", len(v.def.Flow.Nodes), func(i int) string {
			return v.def.Flow.Nodes[i].ID
		})
	}
}

func (v *validator) checkUniqueIDs(parentPath, field string, n int, get func(int) string) {
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		id := get(i)
		if id == "" {
			continue
		}
		if first, dup := seen[id]; dup {
			v.errf(diagnostic.CodeDuplicateID,
				fmt.Sprintf("%s.%d.%s", parentPath, i, field),
				"duplicate %s %q (first declared at %s[%d])", field, id, parentPath, first)
			continue
		}
		seen[id] = i
	}
}
