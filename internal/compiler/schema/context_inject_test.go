package schema

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The `context:` block must parse from YAML at BOTH app level and agent level —
// the declarative interface app authors actually write.
func TestContextBlock_YAMLRoundTrip(t *testing.T) {
	src := `
app:
  app_id: demo
  name: Demo
context:
  sections:
    - id: user
      builtin: user
      priority: 1
    - id: greeting
      title: Context
      template: "Hello {{user.name}} from {{user.region}}."
      priority: 2
    - id: eu
      text: "GDPR applies."
      when: user.region
      priority: 3
agents:
  - id: main
    brain:
      model: gpt-4o-mini
    context:
      sections:
        - id: policy
          text: "Be concise."
          priority: 4
`
	var def AppDefinition
	if err := yaml.Unmarshal([]byte(src), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.Context == nil || len(def.Context.Sections) != 3 {
		t.Fatalf("app context not parsed: %+v", def.Context)
	}
	if def.Context.Sections[0].Builtin != "user" || def.Context.Sections[1].Template == "" || def.Context.Sections[2].When != "user.region" {
		t.Errorf("app section fields wrong: %+v", def.Context.Sections)
	}
	if len(def.Agents) != 1 || def.Agents[0].Context == nil || len(def.Agents[0].Context.Sections) != 1 {
		t.Fatalf("agent context not parsed: %+v", def.Agents)
	}
	if def.Agents[0].Context.Sections[0].Text != "Be concise." {
		t.Errorf("agent section wrong: %+v", def.Agents[0].Context.Sections[0])
	}
}
