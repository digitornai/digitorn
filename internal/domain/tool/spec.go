// Package tool defines the schema of tools exposed by modules to LLMs and
// the runtime types involved in invoking them.
//
// A *tool* is one callable function on a module (e.g. `filesystem.read`).
// The Spec describes the tool's name, description, parameters and risk
// profile — exactly what the LLM sees in its tool list.
package tool

// RiskLevel classifies a tool's risk profile.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// ParamSpec describes one parameter of a tool.
type ParamSpec struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"` // "string", "integer", "boolean", "array", "object"
	Description string      `json:"description"`
	Required    bool        `json:"required"`
	Default     any         `json:"default,omitempty"`
	Enum        []any       `json:"enum,omitempty"`
	Items       *ParamSpec  `json:"items,omitempty"`      // for arrays
	Properties  []ParamSpec `json:"properties,omitempty"` // for objects
	// Path marks a parameter as a filesystem path subject to the workdir
	// PathPolicy. The dispatch chokepoint resolves/rewrites/rejects such
	// args against the session's workdir BEFORE the module runs, so an
	// agent can never reach outside its workdir regardless of the module.
	Path bool `json:"path,omitempty"`
}

// PathParamNames returns the names of the parameters marked as filesystem
// paths (Path=true) — the args the dispatch chokepoint enforces against the
// workdir.
func (s Spec) PathParamNames() []string {
	var out []string
	for _, p := range s.Params {
		if p.Path {
			out = append(out, p.Name)
		}
	}
	return out
}

// Spec describes one tool callable on a module.
//
// Fields used by the context_builder tool index (CB-1) :
//   - Tags     : free-form classification keywords from @action decorator
//   - Aliases  : multilingual short names (e.g. ["lire", "read file"])
//     that the keyword search expands so non-English queries
//     still find the tool. Documented in
//     docs-site/docs/language/04-tools.md "Semantic search".
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
	// DataClassification is the sensitivity level of the data this action
	// handles (public | internal | confidential | restricted). Gate 5
	// blocks the call when it exceeds the app's max_data_classification.
	// Empty = unclassified (gate 5 never fires for this action).
	DataClassification string `json:"data_classification,omitempty"`
	Internal           bool   `json:"internal,omitempty"` // hidden from LLM tool list
	CLILabel           string `json:"cli_label,omitempty"`
	CLIParam           string `json:"cli_param,omitempty"`
}

// ToJSONSchema converts the tool spec into a JSON Schema object suitable for
// passing to an LLM provider as a tool definition.
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
