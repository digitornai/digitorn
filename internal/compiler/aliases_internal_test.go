package compiler

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func TestNormalize_ToolsModulesRenamed(t *testing.T) {
	def := &schema.AppDefinition{Tools: &schema.ToolsBlock{
		Modules: map[string]schema.ModuleBlock{
			"workspace": {Config: map[string]any{"workspace": "."}},
		},
	}}
	normalizeModuleAliases(def)
	if _, ok := def.Tools.Modules["workspace"]; ok {
		t.Fatal("workspace key must be gone after alias")
	}
	blk, ok := def.Tools.Modules["filesystem"]
	if !ok {
		t.Fatal("filesystem key must exist after alias")
	}
	if blk.Config["workspace"] != "." {
		t.Fatalf("config must carry over, got %+v", blk.Config)
	}
}

func TestNormalize_ToolsModulesMerge_ExplicitTargetWins(t *testing.T) {
	def := &schema.AppDefinition{Tools: &schema.ToolsBlock{
		Modules: map[string]schema.ModuleBlock{
			"workspace":  {Config: map[string]any{"workspace": "legacy"}},
			"filesystem": {Config: map[string]any{"workspace": "explicit"}},
		},
	}}
	normalizeModuleAliases(def)
	if _, ok := def.Tools.Modules["workspace"]; ok {
		t.Fatal("workspace key must be gone")
	}
	if got := def.Tools.Modules["filesystem"].Config["workspace"]; got != "explicit" {
		t.Fatalf("explicit filesystem block must win, got %v", got)
	}
}

func TestNormalize_AgentModulesDedup_AllToolsWins(t *testing.T) {
	def := &schema.AppDefinition{Agents: []schema.Agent{{
		ID: "main",
		Modules: schema.AgentModules{
			{ID: "workspace", Tools: []string{"read", "write"}},
			{ID: "filesystem"}, // empty Tools = all tools
		},
	}}}
	normalizeModuleAliases(def)
	mods := def.Agents[0].Modules
	if len(mods) != 1 {
		t.Fatalf("aliased duplicate must merge to 1 ref, got %d (%+v)", len(mods), mods)
	}
	if mods[0].ID != "filesystem" {
		t.Fatalf("ref id = %q", mods[0].ID)
	}
	if len(mods[0].Tools) != 0 {
		t.Fatalf("an all-tools ref must dominate the subset, got tools %v", mods[0].Tools)
	}
}

func TestNormalize_AgentModulesDedup_UnionSubsets(t *testing.T) {
	def := &schema.AppDefinition{Agents: []schema.Agent{{
		ID: "main",
		Modules: schema.AgentModules{
			{ID: "workspace", Tools: []string{"read", "write"}},
			{ID: "filesystem", Tools: []string{"write", "grep"}},
		},
	}}}
	normalizeModuleAliases(def)
	mods := def.Agents[0].Modules
	if len(mods) != 1 || mods[0].ID != "filesystem" {
		t.Fatalf("must merge to one filesystem ref, got %+v", mods)
	}
	got := map[string]bool{}
	for _, t := range mods[0].Tools {
		got[t] = true
	}
	for _, want := range []string{"read", "write", "grep"} {
		if !got[want] {
			t.Fatalf("merged tools missing %q: %v", want, mods[0].Tools)
		}
	}
	if len(mods[0].Tools) != 3 {
		t.Fatalf("union must dedup 'write', got %v", mods[0].Tools)
	}
}

func TestNormalize_CapabilitiesAllSlices(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant:         []schema.CapabilityGrant{{Module: "workspace", Tools: []string{"write"}}},
		Approve:       []schema.CapabilityGrant{{Module: "workspace", Tools: []string{"delete"}}},
		Deny:          []schema.CapabilityGrant{{Module: "workspace"}},
		HiddenActions: []schema.CapabilityGrant{{Module: "workspace", Tools: []string{"grep"}}},
		HiddenModules: []string{"workspace", "shell"},
		RateLimits:    map[string]int{"workspace.write": 5, "shell.exec": 2},
	}
	def := &schema.AppDefinition{Tools: &schema.ToolsBlock{Capabilities: caps}}
	normalizeModuleAliases(def)

	for _, g := range [][]schema.CapabilityGrant{caps.Grant, caps.Approve, caps.Deny, caps.HiddenActions} {
		if g[0].Module != "filesystem" {
			t.Fatalf("grant module not aliased: %q", g[0].Module)
		}
	}
	if caps.HiddenModules[0] != "filesystem" || caps.HiddenModules[1] != "shell" {
		t.Fatalf("hidden_modules wrong: %v", caps.HiddenModules)
	}
	if _, ok := caps.RateLimits["workspace.write"]; ok {
		t.Fatal("rate_limits key must be re-keyed off workspace")
	}
	if caps.RateLimits["filesystem.write"] != 5 || caps.RateLimits["shell.exec"] != 2 {
		t.Fatalf("rate_limits wrong: %v", caps.RateLimits)
	}
}

func TestNormalize_DirectModulesAndNonAliasUntouched(t *testing.T) {
	def := &schema.AppDefinition{
		Runtime: &schema.RuntimeBlock{DirectModules: []string{"workspace", "shell", "memory"}},
		Agents:  []schema.Agent{{ID: "a", Modules: schema.AgentModules{{ID: "shell"}, {ID: "bash"}}}},
	}
	normalizeModuleAliases(def)
	if def.Runtime.DirectModules[0] != "filesystem" {
		t.Fatalf("direct_modules[0] = %q", def.Runtime.DirectModules[0])
	}
	if def.Runtime.DirectModules[1] != "shell" || def.Runtime.DirectModules[2] != "memory" {
		t.Fatalf("non-aliased direct modules changed: %v", def.Runtime.DirectModules)
	}
	if len(def.Agents[0].Modules) != 2 || def.Agents[0].Modules[0].ID != "shell" || def.Agents[0].Modules[1].ID != "bash" {
		t.Fatalf("non-aliased agent modules changed: %+v", def.Agents[0].Modules)
	}
}

func TestNormalize_NilSafe(t *testing.T) {
	normalizeModuleAliases(nil)                      // must not panic
	normalizeModuleAliases(&schema.AppDefinition{})  // empty def, no Tools/Runtime/Agents
}
