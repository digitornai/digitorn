package schema

// ContextBlock declares system-prompt context sections injected FRESH every turn —
// so user / session data is never baked into the per-agent cached prompt (no leak
// across users). App-level (AppDefinition.Context) applies to every agent ;
// agent-level (Agent.Context) extends it and overrides a section sharing an id.
type ContextBlock struct {
	Sections []ContextSection `yaml:"sections,omitempty"`
}

// ContextSection is one injected block. Exactly one source is used, in order of
// precedence: `builtin` → `file`/`files` → `template` → `text`.
// `when` gates rendering on a data-bag path (dropped when empty/false).
// Lower `priority` renders first.
type ContextSection struct {
	ID       string   `yaml:"id,omitempty"`
	Title    string   `yaml:"title,omitempty"`
	Text     string   `yaml:"text,omitempty"`
	Template string   `yaml:"template,omitempty"`
	Builtin  string   `yaml:"builtin,omitempty"`
	File     string   `yaml:"file,omitempty"`     // single file (relative to workdir or absolute)
	Files    []string `yaml:"files,omitempty"`    // multiple files — merged with file:
	Dir      string   `yaml:"dir,omitempty"`      // load all *.md files from this directory
	Optional bool     `yaml:"optional,omitempty"` // silently skip missing/unreadable files
	Writable bool     `yaml:"writable,omitempty"` // agent may write back — injects memory writing directive
	When     string   `yaml:"when,omitempty"`
	Priority int      `yaml:"priority,omitempty"`
}
