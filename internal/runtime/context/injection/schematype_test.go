package injection

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

func TestJSONSchemaType_ValidForProviders(t *testing.T) {
	cases := map[string]string{
		"string_list":           "array",
		"string":                "string",
		"regex":                 "string",
		"integer":               "integer",
		"int":                   "integer",
		"boolean":               "boolean",
		"number":                "number",
		"object":                "object",
		"unknown_made_up":       "string",
	}
	for in, want := range cases {
		got := jsonSchemaType(in)
		if got["type"] != want {
			t.Errorf("jsonSchemaType(%q).type = %v, want %v", in, got["type"], want)
		}
	}
	// string_or_string_list uses anyOf, no top-level type.
	if _, ok := jsonSchemaType("string_or_string_list")["anyOf"]; !ok {
		t.Error("string_or_string_list should produce an anyOf")
	}
}

// The generated schema for a string_list param must contain only JSON-Schema
// standard type names — never the internal "string_list".
func TestParamSchema_NoInternalTypeLeaks(t *testing.T) {
	s := paramsToJSONSchema([]tool.ParamSpec{
		{Name: "extensions", Type: "string_list", Description: "x"},
		{Name: "match", Type: "string_or_string_list", Description: "y"},
	})
	b, _ := json.Marshal(s)
	for _, bad := range []string{"string_list", "string_or_string_list", "regex"} {
		if strings.Contains(string(b), `"`+bad+`"`) {
			t.Errorf("generated schema leaks internal type %q: %s", bad, b)
		}
	}
}
