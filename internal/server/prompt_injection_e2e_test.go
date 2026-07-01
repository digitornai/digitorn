package server

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/injection"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
	"github.com/digitornai/digitorn/pkg/module"
)

// buildRegistryWith registers + starts a single module instance (which may
// implement PromptContributor) so registryContributors.Get returns that exact
// instance and the type-assertion to PromptContributor succeeds.
func buildRegistryWith(t *testing.T, m domainmodule.Module) *module.Registry {
	t.Helper()
	reg := module.NewRegistry()
	if err := reg.Register(func() domainmodule.Module { return m }); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Start(context.Background(), m.Manifest().ID); err != nil {
		t.Fatalf("start %s: %v", m.Manifest().ID, err)
	}
	return reg
}

// promptModule is a fakeModule that ALSO implements domainmodule.PromptContributor,
// so the production registryContributors.Gather picks up its sections + dynamic
// tool prompts — proving the automatic, zero-wiring injection path end-to-end.
type promptModule struct {
	*fakeModule
	sections []domainmodule.PromptSection
	dynamic  map[string]string
}

func (p *promptModule) PromptSections(domainmodule.PromptScope) []domainmodule.PromptSection {
	return p.sections
}
func (p *promptModule) DynamicToolPrompts(domainmodule.PromptScope) map[string]string {
	return p.dynamic
}

// TestPromptInjection_E2E_ContributorToPrompt walks the FULL production chain
// of the automatic module-driven prompt injection :
//
//	module PromptContributor{PromptSections, DynamicToolPrompts} + tool.Spec.ToolPrompt
//	  → registryContributors.Gather (the real production source, authorization-gated)
//	    → prompt.Assembler (the real system-prompt builder)
//
// It proves end-to-end that an authorized module's contributed sections + a
// dynamic tool-prompt overlay reach an AUTHORIZED agent's system prompt, and
// are COMPLETELY ABSENT from an unauthorized agent's prompt — zero per-module
// wiring. This is the guarantee for every future module.
func TestPromptInjection_E2E_ContributorToPrompt(t *testing.T) {
	fm := newFakeModule("vault", tool.Spec{
		Name:        "open",
		Description: "Open the vault and read a secret.",
		RiskLevel:   tool.RiskLow,
		ToolPrompt:  "VAULT_STATIC_TOOL_PROMPT_zzz",
	})
	pm := &promptModule{
		fakeModule: fm,
		sections: []domainmodule.PromptSection{
			{Title: "Vault", Priority: 50, Content: "VAULT_MODULE_SECTION_zzz"},
		},
		dynamic: map[string]string{"vault.open": "VAULT_DYNAMIC_TOOL_PROMPT_zzz"},
	}
	reg := buildRegistryWith(t, pm)

	// The real production action source + contributor source over one registry.
	actions := registryActions{Registry: reg}
	contributors := registryContributors{Registry: reg}
	universe := actions.ForApp("")

	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	assemble := func(agent *schema.Agent) string {
		idx := index.NewBuilder().Build(true, caps, agent, universe)
		authorized := make([]string, 0, len(idx.Categories))
		for m := range idx.Categories {
			authorized = append(authorized, m)
		}
		secs, dyn := contributors.Gather(domainmodule.PromptScope{AgentID: agent.ID, Role: agent.Role}, authorized)
		return prompt.NewAssembler().Assemble(prompt.PromptContext{
			Agent:              agent,
			InjectionMode:      injection.ModeDiscovery,
			ToolIndex:          idx,
			InjectedTools:      []llm.ToolSpec{{Name: "search_tools", Description: "Discover."}, {Name: "execute_tool", Description: "Execute."}},
			ModuleSections:     secs,
			DynamicToolPrompts: dyn,
		})
	}

	authorized := assemble(&schema.Agent{ID: "a", Role: "assistant", Modules: schema.AgentModules{{ID: "vault"}}})
	restricted := assemble(&schema.Agent{ID: "b", Role: "assistant", Modules: schema.AgentModules{{ID: "other"}}})

	// Authorized agent sees the module section AND the dynamic tool-prompt
	// overlay (which WINS over the static one).
	for _, want := range []string{"vault", "VAULT_MODULE_SECTION_zzz", "VAULT_DYNAMIC_TOOL_PROMPT_zzz"} {
		if !strings.Contains(authorized, want) {
			t.Errorf("authorized agent missing %q\n--- prompt ---\n%s", want, authorized)
		}
	}
	// Dynamic overlay precedence : the static prompt must NOT appear when a
	// dynamic one exists for the same FQN.
	if strings.Contains(authorized, "VAULT_STATIC_TOOL_PROMPT_zzz") {
		t.Errorf("dynamic tool prompt should override static — static leaked\n%s", authorized)
	}
	// Unauthorized agent : zero trace of vault.
	for _, leak := range []string{"vault", "VAULT_MODULE_SECTION_zzz", "VAULT_DYNAMIC_TOOL_PROMPT_zzz", "VAULT_STATIC_TOOL_PROMPT_zzz", "open"} {
		if strings.Contains(restricted, leak) {
			t.Errorf("LEAK: unauthorized agent saw %q\n--- prompt ---\n%s", leak, restricted)
		}
	}
}
