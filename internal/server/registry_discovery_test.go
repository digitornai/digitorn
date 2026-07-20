package server

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// A wildcard grant says "this app may reach all of it", which is a statement
// about REACH, not about what belongs in the prompt. Injecting every schema of a
// large module would spend the token budget on tools the agent will not touch
// this turn — the exact cost the pieces catalog was already avoiding. These
// tests pin that rule as a property of the platform rather than a quirk of one
// module.

func discoverySetFrom(caps *schema.CapabilitiesConfig) map[string]struct{} {
	set := make(map[string]struct{})
	if caps == nil {
		return set
	}
	for _, g := range caps.Grant {
		if g.Module == "" || g.Module == "pieces" {
			continue
		}
		for _, t := range g.EffectiveTools() {
			if t == "*" {
				set[g.Module] = struct{}{}
				break
			}
		}
	}
	return set
}

func TestWildcardGrantMeansDiscoveryOnly(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant: []schema.CapabilityGrant{
			{Module: "web", Tools: []string{"*"}},
			{Module: "filesystem", Tools: []string{"read", "write"}},
			{Module: "preview", Tools: []string{"inspect"}},
		},
	}
	set := discoverySetFrom(caps)

	if _, ok := set["web"]; !ok {
		t.Error("a wildcard grant must make the module discovery-only, or its whole catalog lands in every prompt")
	}
	if _, ok := set["filesystem"]; ok {
		t.Error("an explicit tool list must stay directly injected — the agent uses those every turn")
	}
	if _, ok := set["preview"]; ok {
		t.Error("a single named tool must stay directly injected")
	}
}

func TestPiecesKeepsItsOwnCatalogPath(t *testing.T) {
	// Pieces resolves its actions from the live bridge, with its own
	// wildcard handling. Claiming it here as well would index it twice.
	caps := &schema.CapabilitiesConfig{
		Grant: []schema.CapabilityGrant{{Module: "pieces", Tools: []string{"*"}}},
	}
	if _, ok := discoverySetFrom(caps)["pieces"]; ok {
		t.Error("pieces must be left to its own catalog")
	}
}

func TestNoGrantsMeansNothingHidden(t *testing.T) {
	if len(discoverySetFrom(nil)) != 0 {
		t.Error("an app without capabilities must not hide anything")
	}
	empty := &schema.CapabilitiesConfig{}
	if len(discoverySetFrom(empty)) != 0 {
		t.Error("an app granting nothing must not hide anything")
	}
}
