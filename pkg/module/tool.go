package module

import "github.com/mbathepaul/digitorn/internal/domain/tool"

type Tool struct {
	Name            string
	Description     string
	Params          []tool.ParamSpec
	Permissions     []string
	RiskLevel       tool.RiskLevel
	Irreversible    bool
	RequireApproval bool
	ToolPrompt      string
	Internal        bool
	CLILabel        string
	CLIParam        string
	Tags            []string
	Aliases         []string
	Handler         tool.Handler
}

func (t Tool) toSpec() tool.Spec {
	return tool.Spec{
		Name:         t.Name,
		Description:  t.Description,
		Params:       t.Params,
		Permissions:  t.Permissions,
		RiskLevel:    t.RiskLevel,
		Irreversible: t.Irreversible,
		Tags:         t.Tags,
		Aliases:      t.Aliases,
		ToolPrompt:   t.ToolPrompt,
		Internal:     t.Internal,
		CLILabel:     t.CLILabel,
		CLIParam:     t.CLIParam,
	}
}
