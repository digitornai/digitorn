//go:build mcpintegration

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/injection"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// stdioServer is a no-auth stdio MCP server config for a npm package.
func stdioServer(pkg string) map[string]any {
	return map[string]any{
		"transport": "stdio",
		"command":   "npx",
		"args":      []any{"-y", pkg},
		"sandbox":   map[string]any{"permissions": []any{"process.exec"}},
	}
}

// specsToUniverse converts materialized MCP tool specs into the policy
// AvailableAction universe exactly as the daemon's mcp_catalog does, so the
// per-agent index is built from the same shapes production uses.
func specsToUniverse(specs []tool.Spec) []policy.AvailableAction {
	out := make([]policy.AvailableAction, 0, len(specs))
	for i := range specs {
		mod, act := toolname.SplitFQN(toolname.Canonicalize(specs[i].Name))
		if mod == "" || act == "" {
			continue
		}
		s := specs[i]
		s.Name = mod + "." + act
		out = append(out, policy.AvailableAction{Module: mod, Action: act, Spec: &s})
	}
	return out
}

func wireNames(specs []llm.ToolSpec) map[string]string { // wire name -> canonical
	m := map[string]string{}
	for _, s := range specs {
		m[s.Name] = s.Canonical
	}
	return m
}

// TestAdvanced_MultiServer_FullPipeline drives THREE real no-auth MCP servers
// through the WHOLE digitorn pipeline — connection, tool materialization
// (indexation specs), clean wire naming via the real planner, risk inference,
// resource/prompt tools, the discovery index + search, the security gates, and
// real multi-server invocation — deterministically (no LLM).
//
//	go test -tags mcpintegration -run TestAdvanced_MultiServer ./internal/modules/mcp/ -v
func TestAdvanced_MultiServer_FullPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	cfg := map[string]any{"servers": map[string]any{
		"everything": stdioServer("@modelcontextprotocol/server-everything"),
		"memory":     stdioServer("@modelcontextprotocol/server-memory"),
		"seqthink":   stdioServer("@modelcontextprotocol/server-sequential-thinking"),
	}}
	ctx = module.WithModuleConfig(ctx, cfg)
	m := New()
	defer m.pool.shutdown(context.Background())

	specs := m.LiveTools(ctx)
	if len(specs) == 0 {
		t.Fatal("LiveTools materialized nothing across 3 servers")
	}
	t.Logf("materialized %d MCP tools across everything+memory+seqthink", len(specs))

	// ---- 1. Connection + materialization: every declared server contributed ----
	byServer := map[string]int{}
	for _, s := range specs {
		mod, _ := toolname.SplitFQN(toolname.Canonicalize(s.Name))
		byServer[strings.TrimPrefix(mod, "mcp_")]++
	}
	for _, srv := range []string{"everything", "memory", "seqthink"} {
		if byServer[srv] == 0 {
			t.Errorf("server %q materialized 0 tools (connect/list failed)", srv)
		} else {
			t.Logf("  server %-10s -> %d tools", srv, byServer[srv])
		}
	}

	// ---- 2. Every spec is well-formed: canonical name + non-empty risk ----
	for i := range specs {
		s := specs[i]
		if !strings.HasPrefix(s.Name, "mcp_") || !strings.Contains(s.Name, "__") {
			t.Errorf("spec %q is not the canonical mcp_<server>__<tool> form", s.Name)
		}
		if s.RiskLevel == "" {
			t.Errorf("spec %q has empty RiskLevel — gate 2 would fail closed", s.Name)
		}
	}

	// ---- 3. Resource/prompt tools materialized (everything advertises both) ----
	want := map[string]bool{
		"mcp_everything__echo":           false,
		"mcp_everything__list_prompts":   false,
		"mcp_everything__get_prompt":     false,
		"mcp_everything__list_resources": false,
		"mcp_everything__read_resource":  false,
	}
	for _, s := range specs {
		if _, ok := want[s.Name]; ok {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected materialized tool %q not found", name)
		}
	}

	universe := specsToUniverse(specs)

	// ---- 4. Indexation: the per-agent ToolIndex contains every MCP tool ----
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	agent := &schema.Agent{ID: "main", Modules: schema.AgentModules{{ID: "mcp"}}}
	idx := index.NewBuilder().Build(true, caps, agent, universe)
	if idx == nil || len(idx.Tools) == 0 {
		t.Fatal("index built empty from MCP universe")
	}
	if got, want := len(idx.Tools), len(universe); got != want {
		t.Errorf("index has %d tools, universe has %d (gate filtered MCP tools?)", got, want)
	}
	if idx.Get("mcp_everything.echo") == nil {
		t.Error("mcp_everything.echo missing from the index")
	}

	// ---- 5. Clean wire naming via the REAL planner (bare tool names) ----
	dec := (&injection.Planner{}).Plan(idx, agent, nil)
	names := wireNames(dec.Tools)
	if names["echo"] != "mcp_everything.echo" {
		t.Errorf("echo wire name not clean/mapped: %+v", names["echo"])
	}
	for w := range names {
		if strings.Contains(w, "__") && strings.HasPrefix(w, "mcp_") {
			t.Errorf("wire name %q still uses the long mcp_<server>__<tool> form", w)
		}
	}
	t.Logf("planner shipped %d wire tools (sample clean names: echo, list_prompts, ...)", len(dec.Tools))

	// ---- 6. Discovery: index.Search finds MCP tools by keyword ----
	if hits := idx.Search("echo", 5); len(hits) == 0 {
		t.Error("discovery search for 'echo' returned no MCP tool")
	}
	if hits := idx.Search("prompt", 10); len(hits) == 0 {
		t.Error("discovery search for 'prompt' returned no MCP prompt tool")
	}

	// ---- 7. Security: gate 1a (module admission), gate 1c (allowed_servers),
	//        gate 4 (deny) — all over the REAL materialized specs ----
	t.Run("security_gates", func(t *testing.T) {
		llmCall := func(fqn string) policy.Invocation {
			mod, act := toolname.SplitFQN(fqn)
			return policy.Invocation{Caller: policy.CallerLLM, Module: mod, Action: act}
		}
		// gate 1a: agent declared `mcp` → every mcp_<server> admitted.
		am := policy.ResolveAgentModules(agent.Modules)
		if !am["mcp"].AllActions {
			t.Fatal("agent must declare mcp with AllActions")
		}
		// allowed_servers = [everything] → memory tool denied, everything allowed.
		pcAllow := policy.PolicyContext{
			AppActive:         true,
			Capabilities:      caps,
			AgentModules:      am,
			MCPAllowedServers: map[string]struct{}{"everything": {}},
		}
		if d := policy.RunGates(llmCall("mcp_everything.echo"), withSpec(pcAllow, idx, "mcp_everything.echo")); d.Kind != policy.DecisionAllow {
			t.Errorf("allowed server echo must pass, got %+v", d)
		}
		if d := policy.RunGates(llmCall("mcp_memory.create_entities"), withSpec(pcAllow, idx, "mcp_memory.create_entities")); d.Kind == policy.DecisionAllow {
			t.Errorf("memory tool must be DENIED by allowed_servers=[everything], got allow")
		}
		// gate 4: deny on the umbrella `mcp` blocks every server's tool.
		denyCaps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, Deny: []schema.CapabilityGrant{{Module: "mcp"}}}
		pcDeny := policy.PolicyContext{AppActive: true, Capabilities: denyCaps, AgentModules: am}
		if d := policy.RunGates(llmCall("mcp_everything.echo"), withSpec(pcDeny, idx, "mcp_everything.echo")); d.Kind != policy.DecisionDeny {
			t.Errorf("deny on umbrella mcp must block echo, got %+v", d)
		}
	})

	// ---- 8. Real multi-server invocation (usage) ----
	t.Run("invoke_each_server", func(t *testing.T) {
		// everything.echo
		assertInvoke(t, m, ctx, "mcp_everything__echo", `{"message":"adv-echo-42"}`, "adv-echo-42")
		// everything.add (numeric tool, distinct shape)
		if hasTool(specs, "mcp_everything__add") {
			assertInvoke(t, m, ctx, "mcp_everything__add", `{"a":17,"b":25}`, "42")
		}
		// memory.create_entities then read_graph (stateful knowledge graph)
		if hasTool(specs, "mcp_memory__create_entities") {
			res, err := m.Invoke(ctx, "mcp_memory__create_entities",
				[]byte(`{"entities":[{"name":"Digitorn","entityType":"project","observations":["mcp-core"]}]}`))
			if err != nil || !res.Success {
				t.Errorf("memory.create_entities failed: err=%v res=%+v", err, res)
			}
		}
	})

	// ---- 9. Resources + prompts actually work ----
	t.Run("resources_and_prompts", func(t *testing.T) {
		if hasTool(specs, "mcp_everything__list_resources") {
			res, err := m.Invoke(ctx, "mcp_everything__list_resources", []byte(`{}`))
			if err != nil || !res.Success {
				t.Errorf("list_resources failed: err=%v res=%+v", err, res)
			}
		}
		if hasTool(specs, "mcp_everything__list_prompts") {
			res, err := m.Invoke(ctx, "mcp_everything__list_prompts", []byte(`{}`))
			if err != nil || !res.Success {
				t.Errorf("list_prompts failed: err=%v res=%+v", err, res)
			}
		}
	})

	// ---- 10. NON-TEXT content (image) is preserved, not dropped ----
	t.Run("nontext_image", func(t *testing.T) {
		name := ""
		for _, cand := range []string{"mcp_everything__getTinyImage", "mcp_everything__get-tiny-image"} {
			if hasTool(specs, cand) {
				name = cand
				break
			}
		}
		if name == "" {
			t.Skip("server exposes no tiny-image tool")
		}
		res, err := m.Invoke(ctx, name, []byte(`{}`))
		if err != nil || !res.Success {
			t.Fatalf("%s failed: err=%v res=%+v", name, err, res)
		}
		data := res.Data.(map[string]any)
		if data["status"] == "empty" {
			t.Error("image tool result wrongly marked empty — the non-text-drop bug is back")
		}
		if _, ok := data["images"]; !ok {
			t.Errorf("image tool result must carry an images[] block, got keys: %v", mapKeys(data))
		} else {
			t.Logf("image tool returned %d image block(s) — preserved", len(data["images"].([]map[string]any)))
		}
	})
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hasTool(specs []tool.Spec, name string) bool {
	for i := range specs {
		if specs[i].Name == name {
			return true
		}
	}
	return false
}

func assertInvoke(t *testing.T, m *Module, ctx context.Context, name, args, want string) {
	t.Helper()
	res, err := m.Invoke(ctx, name, []byte(args))
	if err != nil {
		t.Errorf("%s invoke error: %v", name, err)
		return
	}
	if !res.Success {
		t.Errorf("%s not successful: %+v", name, res)
		return
	}
	b, _ := json.Marshal(res.Data)
	if !strings.Contains(string(b), want) {
		t.Errorf("%s result missing %q: %s", name, want, truncate(string(b), 200))
	}
}

// withSpec injects the index-resolved tool.Spec into pc so gate 2 (risk) and the
// spec-dependent gates assess the REAL materialized spec (risk inferred from the
// tool name), exactly as the daemon's evaluator does.
func withSpec(pc policy.PolicyContext, idx *index.ToolIndex, fqn string) policy.PolicyContext {
	if it := idx.Get(fqn); it != nil {
		pc.ToolSpec = &tool.Spec{Name: fqn, RiskLevel: it.RiskLevel, Irreversible: it.Irreversible}
	}
	return pc
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
