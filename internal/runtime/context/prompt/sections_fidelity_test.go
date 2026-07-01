package prompt_test

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
)

func boolp(b bool) *bool { return &b }

// AuthorityPreamble : present for a real agent, absent for the degenerate
// no-agent context (keeps the empty-prompt invariant).
func TestAuthorityPreamble_PresentForAgent_AbsentForNil(t *testing.T) {
	s := prompt.AuthorityPreambleSection{}
	if out := s.Render(prompt.PromptContext{Agent: &schema.Agent{ID: "a"}}); !strings.Contains(out, "SUPERVISOR AUTHORITY") || !strings.Contains(out, "<digitorn-protocol") {
		t.Errorf("preamble missing for real agent: %q", out)
	}
	if out := s.Render(prompt.PromptContext{}); out != "" {
		t.Errorf("preamble must be empty for nil agent, got %q", out)
	}
}

// Communicate (plan_first) : on by default, on when *true, OFF only when
// explicitly *false.
func TestCommunicate_PlanFirstGating(t *testing.T) {
	s := prompt.CommunicateSection{}
	on := s.Render(prompt.PromptContext{Agent: &schema.Agent{ID: "a"}}) // nil PlanFirst → default ON
	if !strings.Contains(on, "How to communicate") || !strings.Contains(on, `type="plan_first"`) {
		t.Errorf("plan_first default-on missing: %q", on)
	}
	if out := s.Render(prompt.PromptContext{Agent: &schema.Agent{ID: "a", PlanFirst: boolp(true)}}); out == "" {
		t.Errorf("plan_first=true should render")
	}
	if out := s.Render(prompt.PromptContext{Agent: &schema.Agent{ID: "a", PlanFirst: boolp(false)}}); out != "" {
		t.Errorf("plan_first=false should suppress, got %q", out)
	}
}

// ModuleSectionsSection : priority-ordered (lower first), titled "# Title",
// empty-content sections dropped, no content → empty section.
func TestModuleSections_PriorityOrderedAndTitled(t *testing.T) {
	s := prompt.ModuleSectionsSection{}
	out := s.Render(prompt.PromptContext{
		ModuleSections: []domainmodule.PromptSection{
			{Title: "Beta", Priority: 90, Content: "BETA_BODY"},
			{Title: "Alpha", Priority: 10, Content: "ALPHA_BODY"},
			{Title: "Empty", Priority: 5, Content: "   "}, // dropped
		},
	})
	if !strings.Contains(out, "# Alpha\nALPHA_BODY") || !strings.Contains(out, "# Beta\nBETA_BODY") {
		t.Errorf("titled blocks missing: %q", out)
	}
	if strings.Index(out, "ALPHA_BODY") > strings.Index(out, "BETA_BODY") {
		t.Errorf("priority order broken (Alpha p10 must precede Beta p90): %q", out)
	}
	if strings.Contains(out, "Empty") {
		t.Errorf("empty-content section must be dropped: %q", out)
	}
	if got := s.Render(prompt.PromptContext{}); got != "" {
		t.Errorf("no module sections → empty, got %q", got)
	}
}

// ToolUsageSection : dynamic overlay WINS over static tool_prompt for the same
// FQN ; tools without any prompt are omitted.
func TestToolUsage_DynamicOverlayWins(t *testing.T) {
	idx := &index.ToolIndex{Tools: map[string]*index.IndexedTool{
		"m.withstatic": {FQN: "m.withstatic", ToolPrompt: "STATIC_X"},
		"m.noprompt":   {FQN: "m.noprompt"},
	}}
	s := prompt.ToolUsageSection{}
	out := s.Render(prompt.PromptContext{
		ToolIndex:          idx,
		DynamicToolPrompts: map[string]string{"m.withstatic": "DYNAMIC_X"},
	})
	if !strings.Contains(out, "# Tool Usage Instructions") {
		t.Errorf("header missing: %q", out)
	}
	if !strings.Contains(out, "DYNAMIC_X") || strings.Contains(out, "STATIC_X") {
		t.Errorf("dynamic must override static: %q", out)
	}
	if strings.Contains(out, "m.noprompt") {
		t.Errorf("tool with no prompt must be omitted: %q", out)
	}
}
