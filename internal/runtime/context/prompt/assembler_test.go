package prompt_test

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/injection"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func metaSpecs() []llm.ToolSpec {
	return []llm.ToolSpec{
		{Name: "search_tools", Description: "Discover tools by query."},
		{Name: "get_tool", Description: "Fetch one tool's schema."},
		{Name: "execute_tool", Description: "Execute a tool by name."},
	}
}

func domainSpecs(n int) []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, llm.ToolSpec{
			Name:        "filesystem__" + name(i),
			Description: "Action " + name(i) + ".",
		})
	}
	return out
}

func TestAssembler_SectionOrder_MatchesDoc(t *testing.T) {
	want := []string{
		"authority_preamble",
		"identity",
		"tool_instructions",
		"structural_hints",
		"operating_guide",
		"communicate",
		"channel_info",
		"skills",
		"agent_pool",
		"memory_snapshot",
		"memory_instructions",
		"module_sections",
		"tool_usage_instructions",
		"user_prompt",
	}
	a := prompt.NewAssembler()
	got := a.SectionIDs()
	if len(got) != len(want) {
		t.Fatalf("section count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIdentity_BasicAgent(t *testing.T) {
	s := prompt.IdentitySection{}
	out := s.Render(prompt.PromptContext{
		Agent: &schema.Agent{ID: "main", Role: "assistant"},
	})
	if !strings.Contains(out, `You are agent "main"`) {
		t.Errorf("missing agent id : %q", out)
	}
	if !strings.Contains(out, "role: assistant") {
		t.Errorf("missing role : %q", out)
	}
}

func TestIdentity_WithAppMeta(t *testing.T) {
	s := prompt.IdentitySection{}
	out := s.Render(prompt.PromptContext{
		Agent:      &schema.Agent{ID: "main"},
		AppName:    "approval-bot",
		AppVersion: "1.0",
	})
	if !strings.Contains(out, "approval-bot") {
		t.Errorf("missing app name : %q", out)
	}
	if !strings.Contains(out, "v1.0") {
		t.Errorf("missing version : %q", out)
	}
}

func TestIdentity_NilAgent_Empty(t *testing.T) {
	s := prompt.IdentitySection{}
	if got := s.Render(prompt.PromptContext{}); got != "" {
		t.Errorf("nil agent → %q, want empty", got)
	}
}

func buildIndex(t *testing.T, n int) *index.ToolIndex {
	t.Helper()
	universe := make([]policy.AvailableAction, 0, n)
	for i := 0; i < n; i++ {
		mod := "filesystem"
		if i%2 == 1 {
			mod = "shell"
		}
		universe = append(universe, policy.AvailableAction{
			Module: mod,
			Action: "act" + name(i),
			Spec: &tool.Spec{
				Name:        mod + ".act" + name(i),
				Description: "Action " + name(i),
				RiskLevel:   tool.RiskLow,
			},
		})
	}
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	return index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)
}

func name(i int) string {
	letters := []byte("abcdefghijklmnopqrstuvwxyz")
	if i < 26 {
		return string(letters[i])
	}
	return string(letters[i/26-1]) + string(letters[i%26])
}

func TestToolInstructions_DirectMode_MentionsCount(t *testing.T) {
	s := prompt.ToolInstructionsSection{}
	idx := buildIndex(t, 5)
	out := s.Render(prompt.PromptContext{
		InjectionMode: injection.ModeDirect,
		ToolIndex:     idx,
		InjectedTools: domainSpecs(5),
	})
	if !strings.Contains(out, "5") {
		t.Errorf("direct mode should mention 5 tools : %q", out)
	}
	if strings.Contains(out, "search_tools") {
		t.Errorf("direct mode should NOT mention search_tools : %q", out)
	}
	if !strings.Contains(out, "filesystem__a") {
		t.Errorf("direct mode should list the injected domain tools : %q", out)
	}
}

func TestToolInstructions_DiscoveryMode_MentionsAllMetaTools(t *testing.T) {
	s := prompt.ToolInstructionsSection{}
	idx := buildIndex(t, 100)
	out := s.Render(prompt.PromptContext{
		InjectionMode: injection.ModeDiscovery,
		ToolIndex:     idx,
		InjectedTools: metaSpecs(),
	})
	for _, want := range []string{
		"search_tools", "get_tool", "execute_tool",
		"HOW TO USE TOOLS", "AVAILABLE DOMAINS", "CRITICAL",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("discovery mode missing %q : %q", want, out)
		}
	}
}

func TestToolInstructions_CompactDirect_MentionsGetTool(t *testing.T) {
	s := prompt.ToolInstructionsSection{}
	idx := buildIndex(t, 50)
	out := s.Render(prompt.PromptContext{
		InjectionMode: injection.ModeCompactDirect,
		ToolIndex:     idx,
		InjectedTools: domainSpecs(50),
	})
	if !strings.Contains(out, "get_tool") {
		t.Errorf("compact_direct should mention get_tool : %q", out)
	}
}

func TestToolInstructions_DefaultMode_FallsBackToDiscovery(t *testing.T) {
	s := prompt.ToolInstructionsSection{}
	idx := buildIndex(t, 5)
	out := s.Render(prompt.PromptContext{
		InjectionMode: "",
		ToolIndex:     idx,
		InjectedTools: metaSpecs(),
	})
	if !strings.Contains(out, "search_tools") {
		t.Errorf("empty mode should default to discovery : %q", out)
	}
}

// TestToolInstructions_NoToolsNoInjected_Empty locks the anti-pollution
// invariant : a pure-chat agent (no injected tools, no discoverable
// universe) gets NO tool text at all.
func TestToolInstructions_NoToolsNoInjected_Empty(t *testing.T) {
	s := prompt.ToolInstructionsSection{}
	out := s.Render(prompt.PromptContext{InjectionMode: injection.ModeDirect})
	if out != "" {
		t.Errorf("no-tools agent must get empty tool instructions, got %q", out)
	}
}

// =====================================================================
// SkillsSection
// =====================================================================

func TestSkills_Empty_RendersNothing(t *testing.T) {
	s := prompt.SkillsSection{}
	if got := s.Render(prompt.PromptContext{}); got != "" {
		t.Errorf("empty skills → %q, want empty", got)
	}
}

func TestSkills_WithEntries(t *testing.T) {
	s := prompt.SkillsSection{}
	out := s.Render(prompt.PromptContext{
		Skills: []prompt.SkillEntry{
			{Name: "review_pr", Description: "Run code review on a pull request"},
			{Name: "deploy"},
		},
	})
	if !strings.Contains(out, "/review_pr") {
		t.Errorf("missing /review_pr : %q", out)
	}
	if !strings.Contains(out, "Run code review") {
		t.Errorf("missing description : %q", out)
	}
	if !strings.Contains(out, "/deploy") {
		t.Errorf("missing /deploy : %q", out)
	}
}

// =====================================================================
// UserPromptSection — must be LAST
// =====================================================================

// The user prompt is wrapped in an "# APP-DEFINED PERSONALITY" header
// (verbatim from the reference daemon) but the body must appear unchanged and
// LAST in the block.
const personalityHeader = "# APP-DEFINED PERSONALITY"

func TestUserPrompt_SystemPromptField(t *testing.T) {
	s := prompt.UserPromptSection{}
	out := s.Render(prompt.PromptContext{
		Agent: &schema.Agent{SystemPrompt: "Be concise and helpful."},
	})
	if !strings.Contains(out, personalityHeader) || !strings.HasSuffix(out, "Be concise and helpful.") {
		t.Errorf("out = %q", out)
	}
}

func TestUserPrompt_PromptAliasFallback(t *testing.T) {
	s := prompt.UserPromptSection{}
	out := s.Render(prompt.PromptContext{
		Agent: &schema.Agent{Prompt: "Legacy prompt field."},
	})
	if !strings.HasSuffix(out, "Legacy prompt field.") {
		t.Errorf("out = %q", out)
	}
}

func TestUserPrompt_SystemPromptWinsOverLegacy(t *testing.T) {
	s := prompt.UserPromptSection{}
	out := s.Render(prompt.PromptContext{
		Agent: &schema.Agent{
			SystemPrompt: "New.",
			Prompt:       "Legacy.",
		},
	})
	if !strings.HasSuffix(out, "New.") || strings.Contains(out, "Legacy.") {
		t.Errorf("expected SystemPrompt to win, got %q", out)
	}
}

func TestUserPrompt_TrimWhitespace(t *testing.T) {
	s := prompt.UserPromptSection{}
	out := s.Render(prompt.PromptContext{
		Agent: &schema.Agent{SystemPrompt: "  hello  \n"},
	})
	if !strings.HasSuffix(out, "hello") {
		t.Errorf("whitespace not trimmed : %q", out)
	}
}

// =====================================================================
// Full assembly
// =====================================================================

// TestAssemble_UserPromptIsLast : the doc invariant. The user's
// system_prompt MUST be the last non-empty block in the assembled
// output — anything after would override the user's intent.
func TestAssemble_UserPromptIsLast(t *testing.T) {
	a := prompt.NewAssembler()
	idx := buildIndex(t, 3)
	out := a.Assemble(prompt.PromptContext{
		Agent: &schema.Agent{
			ID:           "main",
			Role:         "assistant",
			SystemPrompt: "MARKER_USER_PROMPT_END",
		},
		AppName:       "demo",
		InjectionMode: injection.ModeDirect,
		ToolIndex:     idx,
	})
	if !strings.HasSuffix(out, "MARKER_USER_PROMPT_END") {
		t.Errorf("user_prompt not last : tail = %q",
			out[max(0, len(out)-50):])
	}
}

// TestAssemble_DoubleNewlineSeparator : non-empty sections are
// joined with exactly one blank line. No triple-newlines.
func TestAssemble_DoubleNewlineSeparator(t *testing.T) {
	a := prompt.NewAssembler()
	idx := buildIndex(t, 3)
	out := a.Assemble(prompt.PromptContext{
		Agent: &schema.Agent{
			ID:           "main",
			SystemPrompt: "User text.",
		},
		InjectionMode: injection.ModeDirect,
		ToolIndex:     idx,
	})
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("triple newline found in output : %q", out)
	}
}

// TestAssemble_NilContextStillEmitsMetaToolsBlock : even with no
// agent / no tool index / no user prompt, the discovery-mode
// fallback inside ToolInstructionsSection produces the meta-tools
// reference block. That's correct : the 10 always-available
// primitives are usable from any prompt configuration, so the LLM
// needs to know about them even in degenerate cases.
//
// The placeholder memory / agent_pool / channels sections still
// produce empty output (no double-newline pollution).
func TestAssemble_EmptyContext_NoToolPollution(t *testing.T) {
	a := prompt.NewAssembler()
	out := a.Assemble(prompt.PromptContext{})
	// Anti-pollution : with no agent, no tools and no injected tools the
	// assembler must NOT invent a tool/meta-tools block. A pure-chat (or
	// degenerate) context yields an empty prompt body.
	if strings.Contains(out, "search_tools") || strings.Contains(out, "execute_tool") ||
		strings.Contains(out, "HOW TO USE TOOLS") {
		t.Errorf("empty context must not advertise tools, got %q", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("placeholder sections should drop cleanly, got triple newline in %q", out)
	}
}

// TestAssemble_FullExample : a realistic end-to-end assembly with
// a configured agent, tool index, and user prompt. Verifies the
// expected sections are present and in order.
func TestAssemble_FullExample(t *testing.T) {
	a := prompt.NewAssembler()
	idx := buildIndex(t, 5)
	ctx := prompt.PromptContext{
		Agent: &schema.Agent{
			ID:           "main",
			Role:         "assistant",
			Specialty:    "Code review",
			SystemPrompt: "Be concise.",
		},
		AppName:       "approval-bot",
		AppVersion:    "1.0",
		InjectionMode: injection.ModeDiscovery,
		ToolIndex:     idx,
		Skills: []prompt.SkillEntry{
			{Name: "review_pr", Description: "Run code review"},
		},
	}
	out := a.Assemble(ctx)

	// Order : identity first, then tool_instructions, then skills, then user.
	idxIdentity := strings.Index(out, `You are agent "main"`)
	idxTools := strings.Index(out, "meta-tools")
	idxSkills := strings.Index(out, "/review_pr")
	idxUser := strings.LastIndex(out, "Be concise.")

	if idxIdentity == -1 || idxTools == -1 || idxSkills == -1 || idxUser == -1 {
		t.Fatalf("missing section in output : %s", out)
	}
	if !(idxIdentity < idxTools && idxTools < idxSkills && idxSkills < idxUser) {
		t.Errorf("section order broken : identity=%d tools=%d skills=%d user=%d\n%s",
			idxIdentity, idxTools, idxSkills, idxUser, out)
	}
}

// max is a tiny helper for inline slice arithmetic.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
