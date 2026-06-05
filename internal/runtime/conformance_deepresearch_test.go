package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
)

// Conformance proof against a REAL old-daemon app.
//
// digitorn-deepresearch is a shipped multi-agent app whose YAML declares BOTH
// the modules we just aligned :
//
//	tools:
//	  modules:
//	    memory:        { config: { working_memory: true, ... } }
//	    agent_spawn:   {}
//	  capabilities:
//	    grant:
//	    - { module: memory,      actions: [set_goal, remember, task_create, task_update] }
//	    - { module: agent_spawn, actions: [agent] }
//
// The grant block is the documentation's own source of truth : the memory
// actions live under the `memory` module (set_goal/remember/task_create/
// task_update) and the delegation action under `agent_spawn` (agent). If our
// alignment is conform, feeding this UNMODIFIED config to the digitorn-go
// runtime must offer the LLM exactly memory.set_goal/remember/task_create/
// task_update + agent_spawn.agent (wire form memory__* / agent_spawn__agent).
//
// We never modify the source app : we read its tools block verbatim and run it
// through the production ContextBuilder → wiring → the tool list the LLM
// actually receives.

// locateDeepResearch finds the real app.yaml without depending on a fixed
// absolute path. Honors DIGITORN_BRIDGE_DIR, then tries the conventional
// sibling-repo layout, then the user's Documents folder. Skips (not fails)
// when the old daemon isn't checked out — so CI without the bridge stays green
// while Paul's machine runs the real proof.
func locateDeepResearch(t *testing.T) string {
	t.Helper()
	rel := filepath.Join("packages", "digitorn", "builtins", "digitorn-deepresearch", "app.yaml")
	var candidates []string
	if d := os.Getenv("DIGITORN_BRIDGE_DIR"); d != "" {
		candidates = append(candidates, filepath.Join(d, rel))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "..", "digitorn-bridge", rel),
	)
	if home := os.Getenv("USERPROFILE"); home != "" {
		candidates = append(candidates, filepath.Join(home, "Documents", "digitorn-bridge", rel))
	}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, "Documents", "digitorn-bridge", rel))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("old-daemon digitorn-deepresearch app.yaml not found; set DIGITORN_BRIDGE_DIR to run this conformance test")
	return ""
}

// offeredToolsForConformance runs the production context-build path with the
// given tools block and returns the exact tool list shipped to the LLM.
func offeredToolsForConformance(t *testing.T, tb *schema.ToolsBlock) []llm.ToolSpec {
	t.Helper()
	app := realDispatchApp()
	app.Definition.Tools = tb

	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-conf")
	lc := &stubLLM{responses: []*llm.ChatResponse{{Content: "done"}}}

	cb, disp := buildRealBus(t, t.TempDir())
	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-conf", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got == nil {
		t.Fatal("LLM was never called")
	}
	return lc.got.Tools
}

func confHasTool(tools []llm.ToolSpec, name string) bool {
	for _, ts := range tools {
		if ts.Name == name {
			return true
		}
	}
	return false
}

// TestConformance_DeepResearch_RealApp_ActivatesDocCanonicalTools : the core
// proof. The UNMODIFIED real app declares memory + agent_spawn ; the runtime
// must offer the doc-canonical memory.* + agent_spawn.agent tools.
func TestConformance_DeepResearch_RealApp_ActivatesDocCanonicalTools(t *testing.T) {
	path := locateDeepResearch(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read real app.yaml: %v", err)
	}

	// Parse ONLY the tools block — the part that drives module gating. yaml.v3
	// ignores every other key, and CapabilityGrant's own UnmarshalYAML handles
	// the {module, actions} grant entries exactly as the daemon does.
	var parsed struct {
		Tools *schema.ToolsBlock `yaml:"tools"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse real tools block: %v", err)
	}
	if parsed.Tools == nil {
		t.Fatal("real app has no tools block — fixture drift")
	}

	// Guard against the source app changing under us : these are the
	// declarations the conformance proof hinges on.
	if _, ok := parsed.Tools.Modules["memory"]; !ok {
		t.Fatal("fixture drift: real app no longer declares tools.modules.memory")
	}
	if _, ok := parsed.Tools.Modules["agent_spawn"]; !ok {
		t.Fatal("fixture drift: real app no longer declares tools.modules.agent_spawn")
	}

	offered := offeredToolsForConformance(t, parsed.Tools)

	// The doc-canonical tools the real app's grant block enumerates, in their
	// OpenAI wire form (dots → underscores).
	want := []string{
		"memory__set_goal",
		"memory__remember",
		"memory__task_create",
		"memory__task_update",
		"agent_spawn__agent",
	}
	for _, n := range want {
		if !confHasTool(offered, n) {
			t.Errorf("real app declares the owning module but %q was NOT offered to the LLM — gating not conform to the documented contract", n)
		}
	}

	// The deepresearch index here has domain tools (filesystem) → direct mode,
	// so the execution primitives are injected (the discovery meta-tools are
	// only injected in compact/discovery — relevance policy).
	for _, n := range []string{"context_builder__run_parallel", "context_builder__background_run"} {
		if !confHasTool(offered, n) {
			t.Errorf("execution primitive %q missing (should be present with domain tools)", n)
		}
	}
	// The non-universal primitives are gated. This test's dispatcher wires no
	// AppCaller / AskUser bridge, so call_app / ask_user must NOT be offered —
	// even though the real app GRANTS ask_user (grant alone isn't enough; the
	// bridge must be wired too). This is the fix for injecting non-functional tools.
	for _, n := range []string{"context_builder__call_app", "context_builder__ask_user"} {
		if confHasTool(offered, n) {
			t.Errorf("primitive %q offered but its bridge is not wired — must be gated", n)
		}
	}

	// And the old `context_builder.agent` / `context_builder.set_goal` names
	// must be GONE (they were the divergence we fixed).
	for _, gone := range []string{"context_builder__agent", "context_builder__set_goal", "context_builder__remember", "context_builder__task_create", "context_builder__task_update"} {
		if confHasTool(offered, gone) {
			t.Errorf("stale non-doc tool %q is still offered — the rename is incomplete", gone)
		}
	}
}

// TestConformance_NoModules_OffersNeither : the negative control. An app that
// declares NEITHER module must be offered NEITHER tool — proving the gate
// actually gates (the tools aren't accidentally always-on).
func TestConformance_NoModules_OffersNeither(t *testing.T) {
	tb := &schema.ToolsBlock{
		Capabilities: &schema.CapabilitiesConfig{
			DefaultPolicy: schema.CapAuto,
			MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		},
	}
	offered := offeredToolsForConformance(t, tb)
	for _, n := range []string{
		"memory__set_goal", "memory__remember", "memory__task_create", "memory__task_update",
		"agent_spawn__agent",
	} {
		if confHasTool(offered, n) {
			t.Errorf("app that declares no memory/agent_spawn must NOT be offered %q (gating leaks)", n)
		}
	}
	// With domain tools present (direct mode), the execution primitives remain.
	if !confHasTool(offered, "context_builder__run_parallel") {
		t.Error("execution primitive run_parallel must be offered when the agent has domain tools")
	}
}

// TestConformance_Memory_PresenceActivates_NoConfigFlag : the crux of the
// alignment. The doc gates on DECLARATION, not on a config flag. An empty
// memory block (no working_memory, no config at all) must still activate the
// tools — proving we removed the invented working_memory gate correctly.
func TestConformance_Memory_PresenceActivates_NoConfigFlag(t *testing.T) {
	tb := &schema.ToolsBlock{
		Modules: map[string]schema.ModuleBlock{"memory": {}},
		Capabilities: &schema.CapabilitiesConfig{
			DefaultPolicy: schema.CapAuto,
			MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		},
	}
	offered := offeredToolsForConformance(t, tb)
	if !confHasTool(offered, "memory__set_goal") {
		t.Error("declaring tools.modules.memory (empty block) must activate the memory tools — the gate must NOT require a config flag")
	}
}

// TestConformance_AgentSpawn_GrantOnlyLoadsModule : the doc shows agent_spawn
// can be loaded via tools.capabilities.grant alone ({module: agent_spawn}),
// not only by a tools.modules entry. Either path must offer the tool.
func TestConformance_AgentSpawn_GrantOnlyLoadsModule(t *testing.T) {
	tb := &schema.ToolsBlock{
		Capabilities: &schema.CapabilitiesConfig{
			DefaultPolicy: schema.CapAuto,
			MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
			Grant:         []schema.CapabilityGrant{{Module: "agent_spawn"}},
		},
	}
	offered := offeredToolsForConformance(t, tb)
	if !confHasTool(offered, "agent_spawn__agent") {
		t.Error("granting {module: agent_spawn} must load the module and offer agent_spawn.agent")
	}
}
