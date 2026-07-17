package compiler_test

import (
	"path/filepath"
	"testing"
)

func TestWorkspaceAlias_CompilesToFilesystem(t *testing.T) {
	c := newCompilerForFixtures(t)
	res, err := c.Compile(filepath.Join("testdata", "valid", "workspace_alias"))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !res.OK() {
		t.Fatalf("legacy workspace app must compile clean via the alias; diagnostics:\n%v", res.Diagnostics)
	}

	def := res.Definition
	if def.Tools == nil || def.Tools.Modules == nil {
		t.Fatal("no tools.modules in compiled def")
	}
	if _, ok := def.Tools.Modules["workspace"]; ok {
		t.Error("tools.modules still has 'workspace' after alias")
	}
	if _, ok := def.Tools.Modules["filesystem"]; !ok {
		t.Error("tools.modules missing 'filesystem' after alias")
	}

	if len(def.Agents) == 0 || len(def.Agents[0].Modules) == 0 {
		t.Fatal("no agent modules")
	}
	if id := def.Agents[0].Modules[0].ID; id != "filesystem" {
		t.Errorf("agent module = %q, want filesystem", id)
	}

	if def.Tools.Capabilities == nil || len(def.Tools.Capabilities.Grant) == 0 {
		t.Fatal("no capability grant")
	}
	if m := def.Tools.Capabilities.Grant[0].Module; m != "filesystem" {
		t.Errorf("grant module = %q, want filesystem", m)
	}
}
