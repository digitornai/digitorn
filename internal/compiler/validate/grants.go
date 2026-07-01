package validate

import (
	"fmt"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func (v *validator) checkGrantOverlap() {
	if v.def.Tools == nil || v.def.Tools.Capabilities == nil {
		return
	}
	c := v.def.Tools.Capabilities
	denied := grantSet(c.Deny)
	for i, g := range c.Grant {
		for _, tool := range g.EffectiveTools() {
			if denied[key(g.Module, tool)] {
				v.errf(diagnostic.CodeImpossibleGrant,
					fmt.Sprintf("tools.capabilities.grant.%d", i),
					"%s.%s is in both grant and deny", g.Module, tool)
			}
		}
		if denied[key(g.Module, "")] && len(g.EffectiveTools()) == 0 {
			v.errf(diagnostic.CodeImpossibleGrant,
				fmt.Sprintf("tools.capabilities.grant.%d", i),
				"module %s is granted but also wholly denied", g.Module)
		}
	}
}

func grantSet(grants []schema.CapabilityGrant) map[string]bool {
	out := map[string]bool{}
	for _, g := range grants {
		tools := g.EffectiveTools()
		if len(tools) == 0 {
			out[key(g.Module, "")] = true
			continue
		}
		for _, t := range tools {
			out[key(g.Module, t)] = true
		}
	}
	return out
}

func key(module, tool string) string { return module + "." + tool }
