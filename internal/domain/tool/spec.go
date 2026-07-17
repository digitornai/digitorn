package tool

type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

type ParamSpec struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Required    bool        `json:"required"`
	Default     any         `json:"default,omitempty"`
	Enum        []any       `json:"enum,omitempty"`
	Items       *ParamSpec  `json:"items,omitempty"`
	Properties  []ParamSpec `json:"properties,omitempty"`
	Path bool `json:"path,omitempty"`
}

func (s Spec) PathParamNames() []string {
	var out []string
	for _, p := range s.Params {
		if p.Path {
			out = append(out, p.Name)
		}
	}
	return out
}

type Spec struct {
	Name         string      `json:"name"`
	Description  string      `json:"description"`
	Params       []ParamSpec `json:"params"`
	Permissions  []string    `json:"permissions,omitempty"`
	RiskLevel    RiskLevel   `json:"risk_level"`
	Irreversible bool        `json:"irreversible"`
	Tags         []string    `json:"tags,omitempty"`
	Aliases      []string    `json:"aliases,omitempty"`
	ToolPrompt   string      `json:"tool_prompt,omitempty"`
	DataClassification string `json:"data_classification,omitempty"`
	Internal           bool   `json:"internal,omitempty"`
	CLILabel           string `json:"cli_label,omitempty"`
	CLIParam           string `json:"cli_param,omitempty"`
}

func (s Spec) ToJSONSchema() map[string]any {
	props := make(map[string]any)
	required := make([]string, 0)
	for _, p := range s.Params {
		props[p.Name] = paramToSchema(p)
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{
		"name":        s.Name,
		"description": s.Description,
		"parameters": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		},
	}
}

func paramToSchema(p ParamSpec) map[string]any {
	m := map[string]any{
		"type":        p.Type,
		"description": p.Description,
	}
	if p.Default != nil {
		m["default"] = p.Default
	}
	if len(p.Enum) > 0 {
		m["enum"] = p.Enum
	}
	if p.Items != nil {
		m["items"] = paramToSchema(*p.Items)
	}
	if len(p.Properties) > 0 {
		nestedProps := make(map[string]any)
		nestedReq := make([]string, 0)
		for _, np := range p.Properties {
			nestedProps[np.Name] = paramToSchema(np)
			if np.Required {
				nestedReq = append(nestedReq, np.Name)
			}
		}
		m["properties"] = nestedProps
		m["required"] = nestedReq
	}
	return m
}
