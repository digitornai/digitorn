package prompt_test

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/injection"
	"github.com/mbathepaul/digitorn/internal/runtime/context/prompt"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// TestPromptAuthz_NeverLeaksUnauthorizedModule is the anti-leak guarantee:
// two agents share one app, but only agent A is authorized for the
// "secretmod" module. The assembled system prompt for agent B must contain
// ZERO trace of secretmod — not its tools, not its tool_prompt, not its
// module section.
//
// Two gates enforce this, both keyed on the per-agent SG-3 index:
//
//   - tool side : SECRET_TOOL_PROMPT lives on the spec ; it only reaches the
//     prompt for tools that survive into the agent's index (idx.Tools).
//   - module side : module sections are gathered by the wiring layer ONLY for
//     modules in the agent's authorized set (idx.Categories) — mirrored here
//     by authorizedSections(). A module B can't see contributes nothing.
//
// Any FUTURE module is gated identically — zero per-module code.
func TestPromptAuthz_NeverLeaksUnauthorizedModule(t *testing.T) {
	universe := []policy.AvailableAction{
		{
			Module: "secretmod", Action: "danger",
			Spec: &tool.Spec{
				Name: "secretmod.danger", Description: "Do a secret dangerous thing.",
				RiskLevel: tool.RiskLow, ToolPrompt: "SECRET_TOOL_PROMPT_xyz",
			},
		},
		{
			Module: "common", Action: "ping",
			Spec: &tool.Spec{
				Name: "common.ping", Description: "Ping something common.", RiskLevel: tool.RiskLow,
			},
		},
	}
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}

	// authorizedSections mimics the wiring gate (registryContributors.Gather):
	// a module section is emitted ONLY for modules present in the per-agent
	// index categories. Marker content per module proves which leaked.
	authorizedSections := func(idx *index.ToolIndex) []domainmodule.PromptSection {
		secs := make([]domainmodule.PromptSection, 0, len(idx.Categories))
		for m := range idx.Categories {
			secs = append(secs, domainmodule.PromptSection{
				Title:   m,
				Content: "MODULE_SECTION_" + m + "_xyz",
			})
		}
		return secs
	}

	build := func(agent *schema.Agent) string {
		idx := index.NewBuilder().Build(true, caps, agent, universe)
		return prompt.NewAssembler().Assemble(prompt.PromptContext{
			Agent:          agent,
			InjectionMode:  injection.ModeDiscovery,
			ToolIndex:      idx,
			InjectedTools:  []llm.ToolSpec{{Name: "search_tools", Description: "Discover tools."}, {Name: "execute_tool", Description: "Execute a tool."}},
			ModuleSections: authorizedSections(idx),
		})
	}

	agentA := &schema.Agent{ID: "privileged", Role: "assistant",
		Modules: schema.AgentModules{{ID: "secretmod"}, {ID: "common"}}}
	agentB := &schema.Agent{ID: "restricted", Role: "assistant",
		Modules: schema.AgentModules{{ID: "common"}}}

	promptA := build(agentA)
	promptB := build(agentB)

	// Agent A (authorized) sees the secret module, its tool, its tool_prompt
	// and its module section.
	for _, want := range []string{"secretmod", "SECRET_TOOL_PROMPT_xyz", "MODULE_SECTION_secretmod_xyz"} {
		if !strings.Contains(promptA, want) {
			t.Errorf("authorized agent A should see %q\n--- prompt A ---\n%s", want, promptA)
		}
	}
	// Agent B (NOT authorized) must see NONE of it — zero leak.
	for _, leak := range []string{"secretmod", "SECRET_TOOL_PROMPT_xyz", "MODULE_SECTION_secretmod_xyz", "danger"} {
		if strings.Contains(promptB, leak) {
			t.Errorf("LEAK: restricted agent B must NOT see %q\n--- prompt B ---\n%s", leak, promptB)
		}
	}
	// But B still sees the module it IS authorized for.
	if !strings.Contains(promptB, "common") || !strings.Contains(promptB, "MODULE_SECTION_common_xyz") {
		t.Errorf("agent B should still see its authorized 'common' module section\n%s", promptB)
	}
}
