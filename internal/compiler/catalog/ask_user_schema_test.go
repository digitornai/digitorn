package catalog

import (
	"strings"
	"testing"
)


func TestAskUserSchemaDeclared(t *testing.T) {
	var spec *struct {
		params  map[string]string // name → type
		formSub map[string]bool
		desc    string
	}
	for _, m := range systemModuleManifests() {
		if m.ID != "context_builder" {
			continue
		}
		for _, tl := range m.Tools {
			if tl.Name != "ask_user" {
				continue
			}
			params := map[string]string{}
			formSub := map[string]bool{}
			for _, p := range tl.Params {
				params[p.Name] = p.Type
				if p.Name == "form" && p.Items != nil {
					for _, f := range p.Items.Properties {
						formSub[f.Name] = true
					}
				}
			}
			spec = &struct {
				params  map[string]string
				formSub map[string]bool
				desc    string
			}{params, formSub, tl.Description}
		}
	}
	if spec == nil {
		t.Fatal("context_builder.ask_user not found in the system catalog")
	}

	want := map[string]string{
		"question": "string", "choices": "array", "allow_multiple": "boolean",
		"allow_custom": "boolean", "min_select": "integer", "max_select": "integer",
		"content": "string", "default": "string", "placeholder": "string",
		"multiline": "boolean", "form": "array", "timeout": "number",
	}
	for name, typ := range want {
		if spec.params[name] != typ {
			t.Errorf("ask_user param %q: want type %q, got %q", name, typ, spec.params[name])
		}
	}
	for _, f := range []string{"name", "label", "type", "options", "allow_custom", "required", "min", "max", "pattern"} {
		if !spec.formSub[f] {
			t.Errorf("ask_user form field object missing property %q", f)
		}
	}
}

func TestAskUserPromptPushesToAsk(t *testing.T) {
	var desc string
	for _, m := range systemModuleManifests() {
		for _, tl := range m.Tools {
			if tl.Name == "ask_user" {
				desc = tl.Description
			}
		}
	}
	for _, must := range []string{
		"ALWAYS ask", "does NOT end the turn", "ambiguous",
		"\"choices\"", "\"allow_multiple\": true", "\"allow_custom\": false",
		"\"content\"", "\"form\"", "\"multiselect\"", "\"boolean\"",
		// an example exercising every field type + validation + bounded multi-select
		"\"email\"", "\"url\"", "\"password\"", "\"date\"", "\"number\"",
		"\"range\"", "\"rating\"", "\"pattern\"", "min_select", "max_select",
	} {
		if !strings.Contains(desc, must) {
			t.Errorf("ask_user prompt missing %q", must)
		}
	}
}


func TestAskUserCarriesToolPrompt(t *testing.T) {
	var tp string
	for _, m := range systemModuleManifests() {
		for _, tl := range m.Tools {
			if tl.Name == "ask_user" {
				tp = tl.ToolPrompt
			}
		}
	}
	if tp == "" {
		t.Fatal("ask_user must carry a ToolPrompt (the per-turn ask-first mandate)")
	}
	for _, must := range []string{"USE IT", "Default to asking", "does NOT end the turn"} {
		if !strings.Contains(tp, must) {
			t.Errorf("ask_user ToolPrompt missing %q", must)
		}
	}
}
