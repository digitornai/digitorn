package filesystem

type symContext struct {
	Symbol  string   `json:"symbol,omitempty"`
	Callers []string `json:"callers,omitempty"`
	Imports []string `json:"imports,omitempty"`
}
