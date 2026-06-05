package wiring_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/wiring"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// PARANOID test harness for the wiring Builder. Targets :
//
//   1. Cache : same (app, version, agent) → 1 build under 1000 turns
//   2. Cache key correctness : different app / version / agent → rebuild
//   3. Invalidate selective vs full
//   4. Per-agent isolation : agent A's index ≠ agent B's index
//   5. Concurrent BuildFor : no data races, deterministic results
//   6. Cache survives many sessions of the same agent
//   7. Sub-agent restriction reflected in built result
//   8. EmbeddingClient failure doesn't poison the cache

// --- helpers --------------------------------------------------------

func paranoidApp(appID, version string) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: appID, Name: appID, Version: version},
			Agents: []schema.Agent{
				{
					ID:           "main",
					Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
					SystemPrompt: "main",
				},
				{
					ID:           "reader",
					Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
					SystemPrompt: "reader",
					Modules: schema.AgentModules{
						{ID: "filesystem", Tools: []string{"read"}},
					},
				},
			},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
				},
			},
		},
	}
}

func paranoidUniverse() []policy.AvailableAction {
	return []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{Name: "filesystem.read",
				Description: "Read a file", RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "write",
			Spec: &tool.Spec{Name: "filesystem.write",
				Description: "Write a file", RiskLevel: tool.RiskMedium}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{Name: "shell.bash",
				Description: "Run bash", RiskLevel: tool.RiskLow}},
	}
}

// countingForApp wraps a fixed universe with an atomic call counter.
type countingForApp struct {
	universe []policy.AvailableAction
	calls    atomic.Int64
}

func (c *countingForApp) ForApp(string) []policy.AvailableAction {
	c.calls.Add(1)
	return c.universe
}

// =====================================================================
// SECTION 1 — Cache hit / miss
// =====================================================================

// TestParanoidWiring_Cache_SameAgent_OneBuildUnder1000Turns : we
// call BuildFor 1000 times with the SAME (app, version, agent_id) ;
// the universe lookup should fire exactly ONCE (cache hit on every
// subsequent turn). Documents the doc invariant that the
// context_builder is built once per (app version, agent) and
// shared across all sessions.
func TestParanoidWiring_Cache_SameAgent_OneBuildUnder1000Turns(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	for i := 0; i < 1000; i++ {
		_, err := b.BuildFor(context.Background(), runtime.ContextRequest{
			App: app, Agent: &app.Definition.Agents[0],
		})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if got := actions.calls.Load(); got != 1 {
		t.Errorf("ForApp called %d times, want 1 (cache miss only on first)", got)
	}
}

// TestParanoidWiring_Cache_VersionChange_Rebuilds : bumping the app
// version invalidates the cache key → next BuildFor rebuilds.
func TestParanoidWiring_Cache_VersionChange_Rebuilds(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	v1 := paranoidApp("app1", "1.0")
	v2 := paranoidApp("app1", "1.1")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: v1, Agent: &v1.Definition.Agents[0],
	})
	if got := actions.calls.Load(); got != 1 {
		t.Fatalf("after v1 : calls=%d, want 1", got)
	}
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: v2, Agent: &v2.Definition.Agents[0],
	})
	if got := actions.calls.Load(); got != 2 {
		t.Errorf("after v2 : calls=%d, want 2 (version change invalidates)", got)
	}
}

// TestParanoidWiring_Cache_DifferentAgent_Rebuilds : two agents of
// the SAME app each get their own index built (they can have
// different capabilities or modules).
func TestParanoidWiring_Cache_DifferentAgent_Rebuilds(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0], // "main"
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[1], // "reader"
	})
	if got := actions.calls.Load(); got != 2 {
		t.Errorf("two agents : calls=%d, want 2", got)
	}
}

// TestParanoidWiring_Cache_DifferentApp_Rebuilds : different
// app_id → different cache key.
func TestParanoidWiring_Cache_DifferentApp_Rebuilds(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	a1 := paranoidApp("app1", "1.0")
	a2 := paranoidApp("app2", "1.0")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: a1, Agent: &a1.Definition.Agents[0],
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: a2, Agent: &a2.Definition.Agents[0],
	})
	if got := actions.calls.Load(); got != 2 {
		t.Errorf("two apps : calls=%d, want 2", got)
	}
}

// =====================================================================
// SECTION 2 — Invalidate semantics
// =====================================================================

func TestParanoidWiring_Invalidate_All_DropsEverything(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[1],
	})
	if got := actions.calls.Load(); got != 2 {
		t.Fatalf("pre-invalidate : calls=%d, want 2", got)
	}
	b.Invalidate("", "", "") // wildcard = drop everything
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[1],
	})
	if got := actions.calls.Load(); got != 4 {
		t.Errorf("post-invalidate-all : calls=%d, want 4 (both rebuilt)", got)
	}
}

func TestParanoidWiring_Invalidate_PerAppID_LeavesOthers(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	a1 := paranoidApp("app1", "1.0")
	a2 := paranoidApp("app2", "1.0")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: a1, Agent: &a1.Definition.Agents[0],
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: a2, Agent: &a2.Definition.Agents[0],
	})
	b.Invalidate("app1", "", "")

	// Re-call : app1 cache was dropped → rebuilds ; app2 cache held → hit.
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: a1, Agent: &a1.Definition.Agents[0],
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: a2, Agent: &a2.Definition.Agents[0],
	})
	if got := actions.calls.Load(); got != 3 {
		t.Errorf("selective invalidate : calls=%d, want 3 (app1 rebuilt, app2 cached)", got)
	}
}

func TestParanoidWiring_Invalidate_PerAgent_LeavesOtherAgent(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0], // main
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[1], // reader
	})
	b.Invalidate("", "", "main")

	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[1],
	})
	if got := actions.calls.Load(); got != 3 {
		t.Errorf("per-agent invalidate : calls=%d, want 3", got)
	}
}

// =====================================================================
// SECTION 3 — Per-agent isolation in the result
// =====================================================================

// TestParanoidWiring_PerAgent_ToolListsDiffer : agent "main" and
// agent "reader" of the same app see DIFFERENT tool lists. main
// has no module restriction → sees filesystem + shell ; reader is
// restricted to filesystem.read.
func TestParanoidWiring_PerAgent_ToolListsDiffer(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	main, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	reader, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[1],
	})

	// Wire-form names are underscored ; runtime dispatcher canonicalises
	// back on inbound tool_calls per docs-site/language/04-tools.md.
	mainHasShell := false
	for _, ts := range main.Tools {
		if ts.Name == "shell__bash" {
			mainHasShell = true
		}
	}
	if !mainHasShell {
		t.Error("main agent should see shell.bash (wire : shell__bash)")
	}
	for _, ts := range reader.Tools {
		if ts.Name == "shell__bash" || ts.Name == "filesystem__write" {
			t.Errorf("reader should NOT see %s", ts.Name)
		}
	}
	// reader's system prompt must use "reader" identity / system_prompt.
	if !strings.HasSuffix(reader.SystemPrompt, "reader") {
		tail := reader.SystemPrompt
		if len(tail) > 60 {
			tail = tail[len(tail)-60:]
		}
		t.Errorf("reader prompt should end with 'reader', got tail %q", tail)
	}
}

// =====================================================================
// SECTION 4 — Concurrent BuildFor : no data races, sync.Once works
// =====================================================================

// TestParanoidWiring_Concurrent_SameKey_SingleBuild : 100 goroutines
// fire BuildFor simultaneously on the SAME key. sync.Once must
// ensure exactly ONE build under contention.
func TestParanoidWiring_Concurrent_SameKey_SingleBuild(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := b.BuildFor(context.Background(), runtime.ContextRequest{
				App: app, Agent: &app.Definition.Agents[0],
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := actions.calls.Load(); got != 1 {
		t.Errorf("concurrent same key : ForApp calls=%d, want 1 (sync.Once)", got)
	}
}

// TestParanoidWiring_Concurrent_DifferentKeys_AllBuilt : 50
// goroutines × 2 agents each = 100 calls across 2 cache keys.
// Exactly 2 builds expected, even under contention.
func TestParanoidWiring_Concurrent_DifferentKeys_AllBuilt(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
				App: app, Agent: &app.Definition.Agents[0], // main
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = b.BuildFor(context.Background(), runtime.ContextRequest{
				App: app, Agent: &app.Definition.Agents[1], // reader
			})
		}()
	}
	wg.Wait()
	if got := actions.calls.Load(); got != 2 {
		t.Errorf("two-keys-many-goroutines : calls=%d, want 2", got)
	}
}

// TestParanoidWiring_Concurrent_ResultsIdentical : when 100
// goroutines fetch the same key, every result must be identical
// (same Tools length + same SystemPrompt).
func TestParanoidWiring_Concurrent_ResultsIdentical(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	app := paranoidApp("app1", "1.0")

	const N = 100
	results := make([]runtime.ContextResult, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
				App: app, Agent: &app.Definition.Agents[0],
			})
			results[i] = res
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i := 1; i < N; i++ {
		if len(results[i].Tools) != len(first.Tools) {
			t.Errorf("g%d : %d tools, g0 : %d", i,
				len(results[i].Tools), len(first.Tools))
		}
		if results[i].SystemPrompt != first.SystemPrompt {
			t.Errorf("g%d prompt differs from g0", i)
		}
		if results[i].Mode != first.Mode {
			t.Errorf("g%d mode=%s, g0 mode=%s", i, results[i].Mode, first.Mode)
		}
	}
}

// =====================================================================
// SECTION 5 — Stress : many apps, many agents, many turns
// =====================================================================

// TestParanoidWiring_Stress_ManyAppsManyAgents : 50 apps × 5 agents
// × 20 turns each = 5000 BuildFor calls. We expect exactly 250
// unique builds (50 × 5), proving the cache scales linearly with
// distinct keys.
func TestParanoidWiring_Stress_ManyAppsManyAgents(t *testing.T) {
	actions := &countingForApp{universe: paranoidUniverse()}
	b := wiring.New(actions)
	const NApps = 50
	const NAgents = 5
	const Turns = 20

	apps := make([]*appmgr.RuntimeApp, NApps)
	for i := 0; i < NApps; i++ {
		appID := fmt.Sprintf("app%d", i)
		apps[i] = paranoidApp(appID, "1.0")
		// Add 5 agents per app (paranoidApp creates 2 ; extend).
		for a := 2; a < NAgents; a++ {
			apps[i].Definition.Agents = append(apps[i].Definition.Agents, schema.Agent{
				ID:           fmt.Sprintf("agent%d", a),
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: fmt.Sprintf("agent%d", a),
			})
		}
	}

	var wg sync.WaitGroup
	for _, app := range apps {
		for _, agent := range app.Definition.Agents {
			agent := agent
			app := app
			for turn := 0; turn < Turns; turn++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := b.BuildFor(context.Background(), runtime.ContextRequest{
						App: app, Agent: &agent,
					})
					if err != nil {
						t.Error(err)
					}
				}()
			}
		}
	}
	wg.Wait()

	want := int64(NApps * NAgents)
	if got := actions.calls.Load(); got != want {
		t.Errorf("stress : ForApp calls=%d, want %d", got, want)
	}
}

// =====================================================================
// SECTION 6 — Nil safety
// =====================================================================

func TestParanoidWiring_NilBuilder_GracefulZero(t *testing.T) {
	var b *wiring.Builder
	res, err := b.BuildFor(context.Background(), runtime.ContextRequest{})
	if err != nil {
		t.Errorf("nil Builder.BuildFor error: %v", err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("nil Builder should return empty result")
	}
}

func TestParanoidWiring_NilActions_NoTools(t *testing.T) {
	b := wiring.New(nil) // no actions source → empty universe
	app := paranoidApp("app1", "1.0")
	res, _ := b.BuildFor(context.Background(), runtime.ContextRequest{
		App: app, Agent: &app.Definition.Agents[0],
	})
	// No action universe → no domain tools → activation-by-relevance injects
	// NO context_builder builtins (pure-chat agent). No module-gated tools here.
	if len(res.Tools) != 0 {
		t.Errorf("nil Actions (no tools) → got %d tools, want 0 (no pollution) : %v", len(res.Tools), res.Tools)
	}
}
