package prompt_test

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/prompt"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

func indexWithToolPrompt(t *testing.T) *index.ToolIndex {
	t.Helper()
	universe := []policy.AvailableAction{
		{Module: "notion", Action: "create_page", Spec: &tool.Spec{
			Name:        "notion.create_page",
			Description: "Create a page in Notion.",
			RiskLevel:   tool.RiskLow,
			ToolPrompt:  "Always call notion.get_page first to copy the exact block schema; never invent fields.",
		}},
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{
			Name: "filesystem.read", Description: "Read a file.", RiskLevel: tool.RiskLow,
		}},
	}
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	return index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)
}

func TestToolUsageSection_InjectsPerToolPrompts(t *testing.T) {
	out := prompt.ToolUsageSection{}.Render(prompt.PromptContext{ToolIndex: indexWithToolPrompt(t)})
	for _, want := range []string{
		"# Tool Usage Instructions",
		"## notion.create_page",
		"never invent fields",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tool-usage section missing %q : %q", want, out)
		}
	}
	// A tool without a ToolPrompt must NOT appear.
	if strings.Contains(out, "filesystem.read") {
		t.Errorf("tool without ToolPrompt should not appear : %q", out)
	}
}

func TestToolUsageSection_EmptyWhenNoPrompts(t *testing.T) {
	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{Name: "filesystem.read", Description: "Read.", RiskLevel: tool.RiskLow}},
	}
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)
	out := prompt.ToolUsageSection{}.Render(prompt.PromptContext{ToolIndex: idx})
	if out != "" {
		t.Errorf("no tool_prompt => empty section, got %q", out)
	}
}

func TestOperatingGuide_GatedByTools(t *testing.T) {
	g := prompt.OperatingGuideSection{}
	// No tools -> empty (pure-chat agent stays lean).
	empty := g.Render(prompt.PromptContext{})
	if empty != "" {
		t.Errorf("operating guide must be empty without tools, got %q", empty)
	}
	// With injected tools -> present.
	withTools := g.Render(prompt.PromptContext{InjectedTools: []llm.ToolSpec{{Name: "search_tools"}}})
	if !strings.Contains(withTools, "How to think") {
		t.Errorf("operating guide missing with tools : %q", withTools)
	}
}
