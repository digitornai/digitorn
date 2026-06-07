package schema

// ContextBlock declares system-prompt context sections injected FRESH every turn —
// so user / session data is never baked into the per-agent cached prompt (no leak
// across users). App-level (AppDefinition.Context) applies to every agent ;
// agent-level (Agent.Context) extends it and overrides a section sharing an id.
type ContextBlock struct {
	Sections []ContextSection `yaml:"sections,omitempty"`
}

// ContextSection is one injected block. Exactly one source is used, in order of
// precedence: `builtin` (a named pre-built contributor: datetime, user, session…),
// else `template` (a string with {{user.name}} / {{date}} / … placeholders filled
// from the turn's data bag), else `text` (verbatim). `when` gates rendering on a
// data-bag path (the section is dropped when the path is empty/false). Lower
// `priority` renders first.
type ContextSection struct {
	ID       string `yaml:"id,omitempty"`
	Title    string `yaml:"title,omitempty"`
	Text     string `yaml:"text,omitempty"`
	Template string `yaml:"template,omitempty"`
	Builtin  string `yaml:"builtin,omitempty"`
	When     string `yaml:"when,omitempty"`
	Priority int    `yaml:"priority,omitempty"`
}
