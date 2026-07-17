package module

import (
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

type (
	Manifest  = domainmodule.Manifest
	Platform  = domainmodule.Platform
	ParamSpec = tool.ParamSpec
	ToolSpec  = tool.Spec
	RiskLevel = tool.RiskLevel
	Result    = tool.Result

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
