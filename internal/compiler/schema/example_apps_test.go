package schema

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped claude-code example apps must keep parsing — including the new
// `context:` block — so a YAML typo there can't silently break the install.
func TestExampleApps_Parse(t *testing.T) {
	for _, rel := range []string{
		"../../../examples/claude_code.yaml",
		"../../../examples/claude-code/app.yaml",
	} {
		data, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var def AppDefinition
		if err := yaml.Unmarshal(data, &def); err != nil {
			t.Fatalf("%s: yaml: %v", rel, err)
		}
		if def.App.AppID != "claude-code" {
			t.Errorf("%s: app_id = %q", rel, def.App.AppID)
		}
		if def.Context == nil || len(def.Context.Sections) == 0 {
			t.Fatalf("%s: context block missing", rel)
		}
		// spot-check the wired sections survived
		var hasUser, hasWorkspace bool
		for _, s := range def.Context.Sections {
			if s.Builtin == "user" {
				hasUser = true
			}
			if s.ID == "workspace" && s.Template != "" {
				hasWorkspace = true
			}
		}
		if !hasUser || !hasWorkspace {
			t.Errorf("%s: expected user builtin + workspace template section", rel)
		}
	}
}
