package module

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
)

// SchemaFromType reflects a config struct into a self-contained JSON Schema
// (map form, ready for Manifest.ConfigSchema). Nested definitions are inlined
// (no $ref) and the struct's fields become the root properties, so a UI can
// render the whole tree without resolving references. Secret fields are marked
// with `jsonschema:"format=password"` on the struct; objects/arrays/enums come
// straight from the Go types + their json / jsonschema tags.
//
// Config fields are OPTIONAL by default: a field is `required` only when its
// struct tag says so (`jsonschema:"required"`). Without this, the reflector
// marks every non-omitempty field required, which would force an app's YAML
// `config:` to restate every field instead of falling back to the Go zero
// value.
func SchemaFromType(v any) map[string]any {
	r := &jsonschema.Reflector{
		DoNotReference:             true,
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
	}
	s := r.Reflect(v)
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
