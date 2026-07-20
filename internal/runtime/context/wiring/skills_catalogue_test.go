package wiring_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

// skillsApp mirrors what the chat builtin declares: a dev block with bundled
// skills, each a command + a human description + a path to its markdown.
func skillsApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "skilled", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "skilled", Name: "Skilled", Version: "1.0"},
			Agents: []schema.Agent{{
				ID:           "main",
				Role:         "assistant",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "Be concise.",
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
				},
			},
			Dev: &schema.DevBlock{
				Skills: []schema.SkillEntry{
					{Command: "/docx", Description: "Create and edit Word documents.", Path: "skills/docx/SKILL.md"},
					{Command: "/pdf", Description: "Read and produce PDF files.", Path: "skills/pdf/SKILL.md"},
				},
			},
		},
	}
}

// The agent is handed the use_skill TOOL, but the catalogue telling it WHAT it
// may invoke was never populated: SkillsSection is registered in the assembler
// and short-circuits to "" on an empty list, so the model saw nothing and could
// only ever use a skill the user picked by hand. Walks the real path:
// app manifest → Builder → assembled system prompt.
func TestBuildFor_SystemPromptListsBundledSkills(t *testing.T) {
	b := wiring.New(&staticActions{})
	app := skillsApp()

	res, err := b.BuildFor(context.Background(), runtime.ContextRequest{
		App:        app,
		Agent:      &app.Definition.Agents[0],
		AppName:    "Skilled",
		AppVersion: "1.0",
	})
	if err != nil {
		t.Fatalf("BuildFor: %v", err)
	}

	if !strings.Contains(res.SystemPrompt, "Available skills (invoke via use_skill)") {
		t.Fatalf("skills catalogue missing from the system prompt:\n%s", res.SystemPrompt)
	}
	for _, want := range []string{
		"/docx", "Create and edit Word documents.",
		"/pdf", "Read and produce PDF files.",
	} {
		if !strings.Contains(res.SystemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// Progressive disclosure: the catalogue carries metadata ONLY. A skill's body
// is fetched on demand through use_skill and must never be inlined here, or
// every turn pays for content it may not need.
func TestBuildFor_SkillBodyNeverInSystemPrompt(t *testing.T) {
	b := wiring.New(&staticActions{})
	app := skillsApp()
	// Marker planted in the PATH: if anything ever resolved and inlined the
	// file, the fragment would surface in the prompt.
	app.Definition.Dev.Skills[0].Path = "skills/docx/SECRET_BODY_MARKER.md"

	res, err := b.BuildFor(context.Background(), runtime.ContextRequest{
		App:        app,
		Agent:      &app.Definition.Agents[0],
		AppName:    "Skilled",
		AppVersion: "1.0",
	})
	if err != nil {
		t.Fatalf("BuildFor: %v", err)
	}
	if strings.Contains(res.SystemPrompt, "SECRET_BODY_MARKER") {
		t.Error("skill path/body leaked into the system prompt — the catalogue must be name+description only")
	}
}

// An app declaring no skills must render no catalogue at all (no stray header).
func TestBuildFor_NoSkillsNoCatalogue(t *testing.T) {
	b := wiring.New(&staticActions{})
	app := skillsApp()
	app.Definition.Dev = nil

	res, err := b.BuildFor(context.Background(), runtime.ContextRequest{
		App:        app,
		Agent:      &app.Definition.Agents[0],
		AppName:    "Skilled",
		AppVersion: "1.0",
	})
	if err != nil {
		t.Fatalf("BuildFor: %v", err)
	}
	if strings.Contains(res.SystemPrompt, "Available skills") {
		t.Error("catalogue header rendered for an app that declares no skills")
	}
}
