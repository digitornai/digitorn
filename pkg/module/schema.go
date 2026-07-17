package module

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
)

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
