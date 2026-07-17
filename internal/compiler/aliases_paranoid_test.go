package compiler

import (
	"reflect"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func fullAliasDef() *schema.AppDefinition {
	return &schema.AppDefinition{
		Runtime: &schema.RuntimeBlock{DirectModules: []string{"workspace", "memory"}},
		Tools: &schema.ToolsBlock{
			Modules: map[string]schema.ModuleBlock{
				"workspace": {Config: map[string]any{"max_file_bytes": 1024}},
				"shell":     {Config: map[string]any{"x": 1}},
			},
			Capabilities: &schema.CapabilitiesConfig{
				Grant:         []schema.CapabilityGrant{{Module: "workspace", Tools: []string{"write"}}, {Module: "shell", Tools: []string{"exec"}}},
				Deny:          []schema.CapabilityGrant{{Module: "workspace", Tools: []string{"delete"}}},
				HiddenModules: []string{"workspace"},
				RateLimits:    map[string]int{"workspace.write": 5, "shell.exec": 9},
			},
		},
		Agents: []schema.Agent{
			{ID: "a", Modules: schema.AgentModules{{ID: "workspace", Tools: []string{"read"}}, {ID: "filesystem", Tools: []string{"grep"}}}},
			{ID: "b", Modules: schema.AgentModules{{ID: "workspace"}, {ID: "memory"}}},
		},
	}
}

func assertFullyAliased(t *testing.T, def *schema.AppDefinition) {
	t.Helper()
	if _, ok := def.Tools.Modules["workspace"]; ok {
		t.Error("tools.modules still has workspace")
	}
	if _, ok := def.Tools.Modules["filesystem"]; !ok {
		t.Error("tools.modules missing filesystem")
	}
	if _, ok := def.Tools.Modules["shell"]; !ok {
		t.Error("non-aliased shell module dropped")
	}
	c := def.Tools.Capabilities
	if c.Grant[0].Module != "filesystem" || c.Grant[1].Module != "shell" {
		t.Errorf("grants: %+v", c.Grant)
	}
	if c.Deny[0].Module != "filesystem" {
		t.Errorf("deny: %+v", c.Deny)
	}
	if len(c.HiddenModules) != 1 || c.HiddenModules[0] != "filesystem" {
		t.Errorf("hidden_modules: %v", c.HiddenModules)
	}
	if _, ok := c.RateLimits["workspace.write"]; ok {
		t.Error("rate_limits still keyed on workspace")
	}
	if c.RateLimits["filesystem.write"] != 5 || c.RateLimits["shell.exec"] != 9 {
		t.Errorf("rate_limits: %v", c.RateLimits)
	}
	if def.Runtime.DirectModules[0] != "filesystem" || def.Runtime.DirectModules[1] != "memory" {
		t.Errorf("direct_modules: %v", def.Runtime.DirectModules)
	}
	am := def.Agents[0].Modules
	if len(am) != 1 || am[0].ID != "filesystem" {
		t.Fatalf("agent a modules: %+v", am)
	}
	tools := map[string]bool{}
	for _, x := range am[0].Tools {
		tools[x] = true
	}
	if !tools["read"] || !tools["grep"] || len(am[0].Tools) != 2 {
		t.Errorf("agent a merged tools: %v", am[0].Tools)
	}
	bm := def.Agents[1].Modules
	if len(bm) != 2 || bm[0].ID != "filesystem" || bm[1].ID != "memory" {
		t.Errorf("agent b modules: %+v", bm)
	}
}

func TestNormalize_FullCombinedDef(t *testing.T) {
	def := fullAliasDef()
	normalizeModuleAliases(def)
	assertFullyAliased(t, def)
}

func TestNormalize_Idempotent(t *testing.T) {
	once := fullAliasDef()
	normalizeModuleAliases(once)

	twice := fullAliasDef()
	normalizeModuleAliases(twice)
	normalizeModuleAliases(twice)

	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("alias pass is not idempotent:\n once=%+v\n twice=%+v", once, twice)
	}
	assertFullyAliased(t, twice)
}

func cleanDef() *schema.AppDefinition {
	return &schema.AppDefinition{
		Tools: &schema.ToolsBlock{
			Modules: map[string]schema.ModuleBlock{"filesystem": {Config: map[string]any{"workspace": "."}}},
			Capabilities: &schema.CapabilitiesConfig{
				Grant:      []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"read"}}},
				RateLimits: map[string]int{"filesystem.write": 3},
			},
		},
		Runtime: &schema.RuntimeBlock{DirectModules: []string{"shell", "memory"}},
		Agents:  []schema.Agent{{ID: "a", Modules: schema.AgentModules{{ID: "filesystem"}, {ID: "shell"}}}},
	}
}

func TestNormalize_AlreadyFilesystemUnchanged(t *testing.T) {
	got := cleanDef()
	normalizeModuleAliases(got)
	if !reflect.DeepEqual(got, cleanDef()) {
		t.Fatalf("clean def was perturbed by the alias pass:\n got=%+v\n want=%+v", got, cleanDef())
	}
}
