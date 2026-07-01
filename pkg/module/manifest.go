package module

import (
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

// Type aliases so module authors and external tooling can use one package.
type (
	Manifest  = domainmodule.Manifest
	Platform  = domainmodule.Platform
	ParamSpec = tool.ParamSpec
	ToolSpec  = tool.Spec
	RiskLevel = tool.RiskLevel
	Result    = tool.Result

	// PromptContributor + its types : a module implements these to inject
	// system-prompt content for AUTHORIZED agents (port of the reference
	// daemon's get_prompt_sections / get_dynamic_tool_prompts).
	PromptContributor = domainmodule.PromptContributor
	PromptSection     = domainmodule.PromptSection
	PromptScope       = domainmodule.PromptScope
)

const (
	PlatformLinux   = domainmodule.PlatformLinux
	PlatformMacOS   = domainmodule.PlatformMacOS
	PlatformWindows = domainmodule.PlatformWindows
)

const (
	RiskLow    = tool.RiskLow
	RiskMedium = tool.RiskMedium
	RiskHigh   = tool.RiskHigh
)
