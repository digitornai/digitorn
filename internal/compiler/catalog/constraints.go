package catalog

import "github.com/mbathepaul/digitorn/pkg/module"

var builtinConstraints = map[string]module.ConstraintType{
	"allowed_commands": module.ConstraintStringList,
	"blocked_commands": module.ConstraintStringList,
	"allowed_paths":    module.ConstraintStringList,
	"blocked_paths":    module.ConstraintStringList,
	"allowed_actions":  module.ConstraintStringList,
	"blocked_actions":  module.ConstraintStringList,
	"allowed_domains":  module.ConstraintStringList,
	"blocked_domains":  module.ConstraintStringList,
	"allowed_hosts":    module.ConstraintStringList,

	"max_file_size":   module.ConstraintSize,
	"max_upload_size": module.ConstraintSize,
	"max_body_size":   module.ConstraintSize,
	"max_memory":      module.ConstraintSize,

	"timeout":         module.ConstraintDuration,
	"max_duration":    module.ConstraintDuration,
	"read_timeout":    module.ConstraintDuration,
	"write_timeout":   module.ConstraintDuration,
	"connect_timeout": module.ConstraintDuration,

	"max_concurrency": module.ConstraintInteger,
	"max_requests":    module.ConstraintInteger,
	"max_results":     module.ConstraintInteger,
	"max_depth":       module.ConstraintInteger,
	"max_tokens":      module.ConstraintInteger,

	"read_only":        module.ConstraintBoolean,
	"require_approval": module.ConstraintBoolean,
}

func (c *Catalog) ConstraintType(name string) (module.ConstraintType, bool) {
	t, ok := builtinConstraints[name]
	return t, ok
}
