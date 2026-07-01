package wiring_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/embeddings"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

// staticActions implements wiring.AvailableActions for tests.
type staticActions struct {
	byApp map[string][]policy.AvailableAction
}

func (s *staticActions) ForApp(appID string) []policy.AvailableAction {
	if s == nil {
		return nil
	}
	return s.byApp[appID]
}

// approvalBotApp reproduces the canonical approval-bot from
// security-01-approval.md (same shape as SG-5 E2E test) so the
// wiring tests assert end-to-end behaviour against a documented
// scenario.
func approvalBotApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "approval-bot", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{
				AppID: "approval-bot", Name: "Approval Bot", Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:           "main",
				Role:         "assistant",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "Be concise and helpful.",
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
				},
			},
		},
	}
}

func sampleUniverse() []policy.AvailableAction {
	return []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{
				Name:        "filesystem.read",
				Description: "Read a file from disk",
				RiskLevel:   tool.RiskLow,
			}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{
				Name:        "shell.bash",
				Description: "Execute a Bash command",
				RiskLevel:   tool.RiskLow,
			}},
	}
}

// =====================================================================
// Builder unit tests
// =====================================================================

// TestBuildFor_ReturnsToolsAndSystemPrompt : the basic happy path.
// Builder produces a ContextResult with both Tools and SystemPrompt
// populated.
func TestBuildFor_ReturnsToolsAndSystemPrompt(t *testing.T) {
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{
		"approval-bot": sampleUniverse(),
	}}
	b := wiring.New(actions)
	app := approvalBotApp()
	agent := &app.Definition.Agents[0]

	res, err := b.BuildFor(context.Background(), runtime.ContextRequest{
		App:        app,
		Agent:      agent,
		AppName:    "Approval Bot",
		AppVersion: "1.0",
	})
	if err != nil {
		t.Fatalf("BuildFor: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Error("Tools empty (should have at least the 10 builtins)")
	}
	if res.SystemPrompt == "" {
		t.Error("SystemPrompt empty")
	}
}

// TestBuildFor_UserPromptIsLast : the doc invariant — the user's
// system_prompt is ALWAYS the last line of the assembled output.
func TestBuildFor_UserPromptIsLast(t *testing.T) {
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{
		"approval-bot": sampleUniverse(),
	}}
	b := wiring.New(actions)
	app := approvalBotApp()
	app.Definition.Agents[0].SystemPrompt = "MARKER_USER_PROMPT_END"

	res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	if !strings.HasSuffix(res.SystemPrompt, "MARKER_USER_PROMPT_END") {
		tail := res.SystemPrompt
		if len(tail) > 60 {
			tail = tail[len(tail)-60:]
		}
		t.Errorf("user prompt not last : ...%q", tail)
	}
}

// TestBuildFor_IncludesIdentitySection : the identity section is
// always present when agent.ID is set.
func TestBuildFor_IncludesIdentitySection(t *testing.T) {
	b := wiring.New(&staticActions{})
	app := approvalBotApp()
	res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
		AppName: "Approval Bot",
	})
	if !strings.Contains(res.SystemPrompt, `You are agent "main"`) {
		t.Errorf("identity section missing : %q", res.SystemPrompt)
	}
	if !strings.Contains(res.SystemPrompt, "Approval Bot") {
		t.Errorf("app name missing in identity : %q", res.SystemPrompt)
	}
}

// TestBuildFor_NoTools_NoBuiltins : the activation-by-relevance policy. With an
// EMPTY action universe (a pure-chat agent), the planner injects NO
// context_builder builtins at all — nothing to discover / run / background.
// (memory / agent / the gated optionals are appended by the wiring only on
// their own activation, which isn't set here.)
func TestBuildFor_NoTools_NoBuiltins(t *testing.T) {
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{}}
	b := wiring.New(actions)
	app := approvalBotApp()
	res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	names := make(map[string]bool, len(res.Tools))
	for _, t := range res.Tools {
		names[t.Name] = true
	}
	for _, gone := range []string{
		"context_builder__search_tools", "context_builder__get_tool",
		"context_builder__execute_tool", "context_builder__list_categories",
		"context_builder__browse_category", "context_builder__run_parallel",
		"context_builder__background_run", "context_builder__call_app",
		"context_builder__ask_user", "context_builder__use_skill",
	} {
		if names[gone] {
			t.Errorf("pure-chat agent (no tools) must NOT be offered %s — context pollution", gone)
		}
	}
}

// TestBuildFor_OptionalPrimitivesGatedByFlags : call_app / ask_user / use_skill
// appear ONLY when the corresponding ContextRequest flag is set (the engine
// sets them from wired-bridge + grant/skills). Fresh builders per case because
// BuildFor caches per (app, version, agent).
func TestBuildFor_OptionalPrimitivesGatedByFlags(t *testing.T) {
	app := approvalBotApp()
	mk := func(in runtime.ContextRequest) map[string]bool {
		in.App, in.Agent = app, &app.Definition.Agents[0]
		res, _ := wiring.New(&staticActions{byApp: map[string][]policy.AvailableAction{}}).
			BuildFor(context.Background(), in)
		names := map[string]bool{}
		for _, ts := range res.Tools {
			names[ts.Name] = true
		}
		return names
	}

	on := mk(runtime.ContextRequest{CallAppEnabled: true, AskUserEnabled: true, UseSkillEnabled: true})
	for _, want := range []string{"context_builder__call_app", "context_builder__ask_user", "context_builder__use_skill"} {
		if !on[want] {
			t.Errorf("flag set but %s not offered", want)
		}
	}
	off := mk(runtime.ContextRequest{})
	for _, gone := range []string{"context_builder__call_app", "context_builder__ask_user", "context_builder__use_skill"} {
		if off[gone] {
			t.Errorf("flag unset but %s still offered", gone)
		}
	}
}

// TestBuildFor_HonoursDeny : an action in capabilities.deny doesn't
// land in Tools. Proves SG-3 → CB-1 → ContextBuilder wirage works
// end-to-end.
func TestBuildFor_HonoursDeny(t *testing.T) {
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{
		"approval-bot": sampleUniverse(),
	}}
	app := approvalBotApp()
	app.Definition.Tools.Capabilities.Deny = []schema.CapabilityGrant{
		{Module: "shell", Tools: []string{"bash"}},
	}
	b := wiring.New(actions)
	res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	// shell.bash is dotted in the index, but emitted on the LLM wire as
	// shell__bash (planner sanitization). Check both to fail if either
	// leaks.
	for _, ts := range res.Tools {
		if ts.Name == "shell.bash" || ts.Name == "shell__bash" {
			t.Errorf("shell.bash visible despite deny : %+v", ts)
		}
	}
}

// TestBuildFor_RuntimeOverride_Discovery : runtime.tool_injection
// in YAML forces the mode regardless of the toolset size.
func TestBuildFor_RuntimeOverride_Discovery(t *testing.T) {
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{
		"approval-bot": sampleUniverse(),
	}}
	app := approvalBotApp()
	app.Definition.Runtime = &schema.RuntimeBlock{
		ToolInjection: schema.ToolInjectionDiscovery,
	}
	b := wiring.New(actions)
	res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	if res.Mode != "discovery" {
		t.Errorf("Mode = %s, want discovery (override)", res.Mode)
	}
	// Discovery mode = builtins only (10) + no domain tools.
	// Builtins are sanitized to context_builder__* on the LLM wire.
	domainTools := 0
	for _, t := range res.Tools {
		if !strings.HasPrefix(t.Name, "context_builder__") {
			domainTools++
		}
	}
	if domainTools != 0 {
		t.Errorf("discovery mode emitted %d domain tools, want 0", domainTools)
	}
}

// TestBuildFor_Cached : the second call for the same (app, agent)
// returns the cached result without re-computing.
func TestBuildFor_Cached(t *testing.T) {
	calls := 0
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{
		"approval-bot": sampleUniverse(),
	}}
	app := approvalBotApp()
	b := wiring.New(&countingActions{inner: actions, count: &calls})
	for i := 0; i < 3; i++ {
		_, err := b.BuildFor(context.Background(), runtime.ContextRequest{
			App: app, Agent: &app.Definition.Agents[0],
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("AvailableActions.ForApp called %d times, want 1 (cache miss only on first)", calls)
	}
}

type countingActions struct {
	inner *staticActions
	count *int
}

func (c *countingActions) ForApp(id string) []policy.AvailableAction {
	*c.count++
	return c.inner.ForApp(id)
}

// TestBuildFor_Invalidate : Invalidate(appID,..) drops the cache so
// the next BuildFor recomputes.
func TestBuildFor_Invalidate(t *testing.T) {
	calls := 0
	app := approvalBotApp()
	b := wiring.New(&countingActions{
		inner: &staticActions{byApp: map[string][]policy.AvailableAction{
			"approval-bot": sampleUniverse(),
		}},
		count: &calls,
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	b.Invalidate("approval-bot", "", "")
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	if calls != 2 {
		t.Errorf("invalidate didn't drop cache : calls=%d, want 2", calls)
	}
}

// TestBuildFor_WithEmbeddings_SemanticAttached : when an
// EmbeddingClient is wired, the resulting index has Semantic set.
// We can't observe Tools change directly (semantic only kicks in
// for searches done THROUGH the index), so we verify by triggering
// a Search via the meta-tool dispatcher in a follow-up CB-6 E2E
// test ; here we just sanity-check the wiring path doesn't panic.
func TestBuildFor_WithEmbeddings_NoPanic(t *testing.T) {
	actions := &staticActions{byApp: map[string][]policy.AvailableAction{
		"approval-bot": sampleUniverse(),
	}}
	b := wiring.New(actions).WithEmbeddings(embeddings.MockClient{})
	app := approvalBotApp()
	_, err := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	if err != nil {
		t.Fatalf("BuildFor with embeddings: %v", err)
	}
}

// TestBuildFor_MemoryReRenderedPerTurn : the cache fix. The expensive,
// session-independent artifacts (index + tool list) are built ONCE per
// (app, version, agent) — but the system prompt is RE-ASSEMBLED every call
// from the live ContextRequest.Memory. So two turns of the SAME (app, agent)
// carrying DIFFERENT working memory must produce DIFFERENT prompts, each
// showing its own memory and never the other's. This is exactly the
// cross-session leak / frozen-memory bug the restructure fixed.
func TestBuildFor_MemoryReRenderedPerTurn(t *testing.T) {
	calls := 0
	app := approvalBotApp()
	b := wiring.New(&countingActions{
		inner: &staticActions{byApp: map[string][]policy.AvailableAction{
			"approval-bot": sampleUniverse(),
		}},
		count: &calls,
	})

	turn := func(goal string) string {
		res, err := b.BuildFor(context.Background(), runtime.ContextRequest{
			App: app, Agent: &app.Definition.Agents[0],
			MemoryEnabled: true,
			Memory:        &prompt.WorkingMemoryView{Goal: goal},
		})
		if err != nil {
			t.Fatalf("BuildFor: %v", err)
		}
		return res.SystemPrompt
	}

	const alpha, beta = "ALPHA_SESSION_GOAL", "BETA_SESSION_GOAL"
	p1 := turn(alpha)
	p2 := turn(beta)

	// Each prompt carries its OWN memory.
	if !strings.Contains(p1, alpha) {
		t.Errorf("turn 1 prompt missing its own goal %q", alpha)
	}
	if !strings.Contains(p2, beta) {
		t.Errorf("turn 2 prompt missing its own goal %q", beta)
	}
	// And NEVER the other turn's memory — the leak the cache caused.
	if strings.Contains(p1, beta) {
		t.Errorf("turn 1 leaked turn 2's goal %q", beta)
	}
	if strings.Contains(p2, alpha) {
		t.Errorf("turn 2 served stale/leaked goal %q (frozen-memory bug)", alpha)
	}
	if p1 == p2 {
		t.Error("identical prompts across turns with different memory — memory is frozen")
	}
	// The expensive part stayed cached : the action universe was resolved
	// exactly once despite two BuildFor calls.
	if calls != 1 {
		t.Errorf("artifacts rebuilt : ForApp called %d times, want 1 (index cached)", calls)
	}
	// And the same per-agent index is served to the dispatcher both turns.
	if idx := b.IndexFor("approval-bot", "main"); idx == nil {
		t.Error("IndexFor returned nil after BuildFor — cached index lost")
	}
}

// TestBuildFor_NilApp_GracefulEmpty : nil App → empty universe → no tools.
// Under the activation-by-relevance policy that means NO builtins (a pure-chat
// agent). The build must not error and must return an empty (non-panicking)
// tool list.
func TestBuildFor_NilApp_GracefulEmpty(t *testing.T) {
	b := wiring.New(nil)
	res, err := b.BuildFor(context.Background(), runtime.ContextRequest{})
	if err != nil {
		t.Fatalf("BuildFor nil: %v", err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("nil App (no tools) → got %d tools, want 0 (no pollution)", len(res.Tools))
	}
}
