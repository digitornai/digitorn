package filesystem

// symContext is the code-graph context attached to a grep match : the
// enclosing symbol of the matched line plus its callers and the file's
// imports. Empty in the no-tree-sitter build. Shared by both builds so
// grep compiles either way.
type symContext struct {
	Symbol  string   `json:"symbol,omitempty"`
	Callers []string `json:"callers,omitempty"`
	Imports []string `json:"imports,omitempty"`
}
