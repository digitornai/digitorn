package hooks_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/pkg/module"

	// Register the filesystem module so the compiler's catalog resolves
	// the apps' `modules: [filesystem]` — mirrors the daemon boot wiring.
	_ "github.com/digitornai/digitorn/internal/modules/filesystem"
)

// =====================================================================
// HK-8 — 50-APP EXHAUSTIVE HOOK GUARANTEE.
//
// The user's mandate : "crée 50 applications avec tous les types de hook
// possible, tester à fond le système de hook de bout en bout ... et une
// garantie que ça marche."
//
// This is the GUARANTEE layer (the live real-LLM layer lives in
// internal/server/live_fifty_hooks_e2e_test.go). It is deterministic,
// exhaustive and uses the REAL stack end-to-end at the hook level :
//
//   YAML  →  real compiler + real catalog (RegistrySource)  →
//   compiled schema.Hook  →  real *hooks.Engine.Fire  →
//   assert FireCount + the action's documented effect.
//
// Each of the 52 apps below exercises ONE (event × condition × action)
// combination. Collectively they cover :
//
//   * all 14 documented conditions (schema.AllHookConditions)
//   * all 15 documented actions    (schema.AllHookActions)
//   * all 15 documented events     (schema.AllHookEvents, incl. aliases)
//
// The condition is fed a payload crafted to SATISFY it (wantFire=true)
// or to MISS it (wantFire=false). The miss cases are the proof that the
// engine truly gates on the condition — and, post the ordering fix, that
// a non-matching condition consumes NEITHER the max_fires budget NOR the
// FireCount. Nothing is stubbed : the fired hooks are the bytes the
// compiler emitted from YAML.
// =====================================================================

// effectKind tags the side effect a positive case must produce so the
// test can assert the engine APPLIED the action, not merely counted it.
type effectKind int

const (
	effNone effectKind = iota
	effGateBlock
	effGateAllow
	effModified // transform_params / transform_result
	effInject   // inject_message
)

type hookSpec struct {
	id       string           // unique app id suffix (also the dir name)
	on       string           // YAML `on:` value (canonical event or alias)
	cond     string           // YAML `condition:` flow value
	act      string           // YAML `action:` flow value
	fire     schema.HookEvent // canonical event fired in the test
	payload  hooks.Payload    // payload crafted for the condition
	wantFire bool             // expect FireCount("h1") == 1
	effect   effectKind       // side effect to assert on a positive case
}

// fiftyAppYAML renders a complete, valid, doc-conform single-agent app
// whose runtime.hooks[] holds exactly one hook (id h1).
func fiftyAppYAML(appID, on, cond, act string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: %s
  name: %s
  version: "1.0.0"

runtime:
  context:
    auto_compact: false
  hooks:
    - id: h1
      "on": %s
      condition: %s
      action: %s

agents:
  - id: main
    role: assistant
    brain:
      provider: openai
      model: gpt-4o-mini
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "Hook coverage agent."
    modules: [filesystem]

tools:
  modules:
    filesystem:
      config: { workspace: "." }
  capabilities:
    default_policy: auto
    grant:
      - module: filesystem
        tools: [read, write, glob, grep]
`, appID, appID, on, cond, act)
}

// fiftyHookSpecs is the 52-app matrix. Read it as the catalogue of
// "what the hook system promises the docs deliver".
func fiftyHookSpecs() []hookSpec {
	fsArgs := map[string]any{"path": "main.go"}
	return []hookSpec{
		// ---- conditions × representative actions (positive) ----
		{"always-log-turnstart", "turn_start",
			`{ type: always }`, `{ type: log, message: "hi", level: info }`,
			schema.HookEventTurnStart, hooks.Payload{}, true, effNone},
		{"never-log-turnstart", "turn_start",
			`{ type: never }`, `{ type: log, message: "hi" }`,
			schema.HookEventTurnStart, hooks.Payload{}, false, effNone},
		// Routed lifecycle events that need explicit matrix coverage.
		{"error-log", "error",
			`{ type: always }`, `{ type: log, message: "err seen" }`,
			schema.HookEventError, hooks.Payload{}, true, effNone},
		{"approval-log", "approval_request",
			`{ type: always }`, `{ type: log, message: "approval seen" }`,
			schema.HookEventApprovalRequest, hooks.Payload{}, true, effNone},
		{"pressure-compact-precompact", "pre_compact",
			`{ type: context_pressure, threshold: 0.5 }`, `{ type: compact_context, strategy: summarize, keep_last: 10 }`,
			schema.HookEventPreCompact, hooks.Payload{TokensUsed: 800, MaxTokens: 1000}, true, effNone},
		{"opentasks-gate-stop", "stop",
			`{ type: expression, expr: "open_tasks > 0" }`, `{ type: gate, allow: false, reason: "finish your tasks: {{tasks.summary}}" }`,
			schema.HookEventStop, hooks.Payload{OpenTasks: 2, TasksSummary: "t1 (in_progress), t2 (pending)"}, true, effGateBlock},
		{"prefinish-alias-gate-stop", "pre_finish",
			`{ type: expression, expr: "open_tasks > 0" }`, `{ type: gate, allow: false, reason: "hold the turn" }`,
			schema.HookEventPreFinish, hooks.Payload{OpenTasks: 1}, true, effGateBlock},
		{"turncount-inject-turnstart", "turn_start",
			`{ type: turn_count, threshold: 1 }`, `{ type: inject_message, content: "remember the goal", role: user }`,
			schema.HookEventTurnStart, hooks.Payload{TurnCount: 1}, true, effInject},
		{"turncount-every-log-turnend", "turn_end",
			`{ type: turn_count, threshold: 2, every: 2 }`, `{ type: log, message: "every-2" }`,
			schema.HookEventTurnEnd, hooks.Payload{TurnCount: 4}, true, effNone},
		{"toolcalls-notify-turnend", "turn_end",
			`{ type: tool_calls, threshold: 2 }`, `{ type: notify, title: "busy", message: "many tools", level: info }`,
			schema.HookEventTurnEnd, hooks.Payload{ToolCallsUsed: 2}, true, effNone},
		{"msgcount-log-turnend", "turn_end",
			`{ type: message_count, threshold: 3 }`, `{ type: log, message: "long session" }`,
			schema.HookEventTurnEnd, hooks.Payload{MessageCount: 3}, true, effNone},
		{"toolname-transformparams-toolstart", "tool_start",
			`{ type: tool_name, match: "filesystem.read" }`, `{ type: transform_params, transformation: { path: "safe.txt" } }`,
			schema.HookEventToolStart, hooks.Payload{ToolName: "filesystem.read", ToolArgs: map[string]any{"path": "decoy.txt"}}, true, effModified},
		{"toolname-glob-gate-toolstart", "tool_start",
			`{ type: tool_name, match: "filesystem.*" }`, `{ type: gate, allow: false, reason: "writes blocked" }`,
			schema.HookEventToolStart, hooks.Payload{ToolName: "filesystem.write"}, true, effGateBlock},
		{"toolname-list-log-toolstart", "tool_start",
			`{ type: tool_name, match: ["filesystem.read", "filesystem.grep"] }`, `{ type: log, message: "read-ish" }`,
			schema.HookEventToolStart, hooks.Payload{ToolName: "filesystem.grep"}, true, effNone},
		{"toolfailed-notify-toolend", "tool_end",
			`{ type: tool_failed }`, `{ type: notify, level: error, message: "tool failed" }`,
			schema.HookEventToolEnd, hooks.Payload{ToolStatus: "errored"}, true, effNone},
		{"content-llm-log-turnend", "turn_end",
			`{ type: content_contains, keyword: "deploy" }`, `{ type: log, level: warn, message: "deploy mentioned" }`,
			schema.HookEventTurnEnd, hooks.Payload{LLMContent: "please deploy now"}, true, effNone},
		{"content-user-inject-turnstart", "turn_start",
			`{ type: content_contains, keyword: "urgent" }`, `{ type: inject_message, content: "prioritise", role: system }`,
			schema.HookEventTurnStart, hooks.Payload{UserMessage: "this is urgent"}, true, effInject},
		{"errortype-log-error", "error",
			`{ type: error_type, match: "Timeout.*" }`, `{ type: log, level: error, message: "timeout seen" }`,
			schema.HookEventError, hooks.Payload{ErrorType: "TimeoutError: deadline exceeded"}, true, effNone},
		{"expr-tokens-compact-turnend", "turn_end",
			`{ type: expression, expr: "tokens_used > 1000" }`, `{ type: compact_context, strategy: truncate, keep_last: 30 }`,
			schema.HookEventTurnEnd, hooks.Payload{TokensUsed: 2000}, true, effNone},
		{"expr-messages-log-turnend", "turn_end",
			`{ type: expression, expr: "messages > 3" }`, `{ type: log, message: "many messages" }`,
			schema.HookEventTurnEnd, hooks.Payload{MessageCount: 5}, true, effNone},
		{"expr-toolfailed-notify-toolend", "tool_end",
			`{ type: expression, expr: "tool_failed" }`, `{ type: notify, level: error, message: "expr failure" }`,
			schema.HookEventToolEnd, hooks.Payload{ToolStatus: "errored"}, true, effNone},
		{"allof-gate-toolstart", "tool_start",
			`{ type: all_of, conditions: [ { type: tool_name, match: "filesystem.write" }, { type: not, condition: { type: tool_failed } } ] }`,
			`{ type: gate, allow: false, reason: "write audit" }`,
			schema.HookEventToolStart, hooks.Payload{ToolName: "filesystem.write", ToolStatus: "completed"}, true, effGateBlock},
		{"anyof-notify-toolend", "tool_end",
			`{ type: any_of, conditions: [ { type: tool_failed }, { type: content_contains, keyword: "error" } ] }`,
			`{ type: notify, level: error, message: "problem" }`,
			schema.HookEventToolEnd, hooks.Payload{ToolStatus: "errored"}, true, effNone},
		{"not-log-toolend", "tool_end",
			`{ type: not, condition: { type: tool_failed } }`, `{ type: log, message: "tool ok" }`,
			schema.HookEventToolEnd, hooks.Payload{ToolStatus: "completed"}, true, effNone},

		// ---- remaining actions (each on a realistic event) ----
		{"transformresult-toolend", "tool_end",
			`{ type: tool_name, match: "filesystem.read" }`, `{ type: transform_result, transformation: { text: "REDACTED" } }`,
			schema.HookEventToolEnd, hooks.Payload{ToolName: "filesystem.read", ToolStatus: "completed", ToolResult: map[string]any{"text": "secret"}}, true, effModified},
		{"moduleaction-toolend", "tool_end",
			`{ type: tool_name, match: "filesystem.write" }`, `{ type: module_action, module: filesystem, action: read, params: { path: "{{tool.params.path}}" } }`,
			schema.HookEventToolEnd, hooks.Payload{ToolName: "filesystem.write", ToolArgs: fsArgs}, true, effNone},
		{"moduleactioninject-toolend", "tool_end",
			`{ type: tool_name, match: "filesystem.write" }`, `{ type: module_action_inject, action: "filesystem.read", params: { path: "notes.txt" }, role: user }`,
			schema.HookEventToolEnd, hooks.Payload{ToolName: "filesystem.write"}, true, effNone},
		{"shell-toolend", "tool_end",
			`{ type: tool_name, match: "filesystem.write" }`, `{ type: shell, command: "echo {{tool.params.path}}", on_error: log }`,
			schema.HookEventToolEnd, hooks.Payload{ToolName: "filesystem.write", ToolArgs: fsArgs}, true, effNone},
		{"pipe-toolend", "tool_end",
			`{ type: tool_name, match: "web.fetch" }`, `{ type: pipe, to: web.extract, map: { html: "{{tool.result.text}}" }, extra: { max_links: 10 }, on_error: log }`,
			schema.HookEventToolEnd, hooks.Payload{ToolName: "web.fetch", ToolResult: map[string]any{"text": "<html/>"}}, true, effNone},
		{"chain-toolend", "tool_end",
			`{ type: tool_failed }`, `{ type: chain, actions: [ { type: log, level: error, message: "failed" }, { type: notify, level: error, message: "{{tool.error}}" } ] }`,
			schema.HookEventToolEnd, hooks.Payload{ToolStatus: "errored", ToolError: "boom"}, true, effNone},
		{"lspdiagnose-toolend", "tool_end",
			`{ type: tool_name, match: "filesystem.write" }`, `{ type: lsp_diagnose, path_field: tool.params.path, content_field: tool.params.content, publish: true, inject_result: true }`,
			schema.HookEventToolEnd, hooks.Payload{ToolName: "filesystem.write", ToolArgs: fsArgs}, true, effNone},
		{"compileyaml-toolend", "tool_end",
			`{ type: always }`, `{ type: compile_yaml, path: "app.yaml" }`,
			schema.HookEventToolEnd, hooks.Payload{}, true, effNone},
		{"autotestdeploy-toolend", "tool_end",
			`{ type: always }`, `{ type: auto_test_deploy }`,
			schema.HookEventToolEnd, hooks.Payload{}, true, effNone},

		// ---- event coverage : aliases + lifecycle events ----
		{"alias-pretooluse-log", "pre_tool_use",
			`{ type: always }`, `{ type: log, message: "pre" }`,
			schema.HookEventToolStart, hooks.Payload{}, true, effNone},
		{"alias-posttooluse-log", "post_tool_use",
			`{ type: always }`, `{ type: log, message: "post" }`,
			schema.HookEventToolEnd, hooks.Payload{}, true, effNone},
		{"alias-userprompt-inject", "user_prompt",
			`{ type: always }`, `{ type: inject_message, content: "context note", role: user }`,
			schema.HookEventTurnStart, hooks.Payload{}, true, effInject},
		{"sessionstart-log", "session_start",
			`{ type: always }`, `{ type: log, message: "session up" }`,
			schema.HookEventSessionStart, hooks.Payload{}, true, effNone},
		{"sessionend-notify", "session_end",
			`{ type: always }`, `{ type: notify, message: "session closed" }`,
			schema.HookEventSessionEnd, hooks.Payload{}, true, effNone},
		{"approvalrequest-log", "approval_request",
			`{ type: always }`, `{ type: log, message: "approval asked" }`,
			schema.HookEventApprovalRequest, hooks.Payload{}, true, effNone},
		{"agentspawn-log", "agent_spawn",
			`{ type: always }`, `{ type: log, message: "spawned" }`,
			schema.HookEventAgentSpawn, hooks.Payload{}, true, effNone},
		{"agentcomplete-log", "agent_complete",
			`{ type: always }`, `{ type: log, message: "done" }`,
			schema.HookEventAgentComplete, hooks.Payload{}, true, effNone},
		{"activation-log", "activation",
			`{ type: always }`, `{ type: log, message: "activated" }`,
			schema.HookEventActivation, hooks.Payload{}, true, effNone},

		// ---- negative cases : condition must gate (FireCount stays 0) ----
		{"neg-toolname-miss", "tool_start",
			`{ type: tool_name, match: "shell.bash" }`, `{ type: log, message: "x" }`,
			schema.HookEventToolStart, hooks.Payload{ToolName: "filesystem.read"}, false, effNone},
		{"neg-toolfailed-miss", "tool_end",
			`{ type: tool_failed }`, `{ type: log, message: "x" }`,
			schema.HookEventToolEnd, hooks.Payload{ToolStatus: "completed"}, false, effNone},
		{"neg-pressure-miss", "turn_end",
			`{ type: context_pressure, threshold: 0.9 }`, `{ type: compact_context, strategy: summarize, keep_last: 5 }`,
			schema.HookEventTurnEnd, hooks.Payload{TokensUsed: 300, MaxTokens: 1000}, false, effNone},
		{"neg-turncount-miss", "turn_end",
			`{ type: turn_count, threshold: 5 }`, `{ type: log, message: "x" }`,
			schema.HookEventTurnEnd, hooks.Payload{TurnCount: 2}, false, effNone},
		{"neg-expr-miss", "turn_end",
			`{ type: expression, expr: "tokens_used > 1000" }`, `{ type: log, message: "x" }`,
			schema.HookEventTurnEnd, hooks.Payload{TokensUsed: 500}, false, effNone},
		{"neg-content-miss", "turn_end",
			`{ type: content_contains, keyword: "zzz" }`, `{ type: log, message: "x" }`,
			schema.HookEventTurnEnd, hooks.Payload{LLMContent: "hello world"}, false, effNone},
		{"neg-not-always", "turn_start",
			`{ type: not, condition: { type: always } }`, `{ type: log, message: "x" }`,
			schema.HookEventTurnStart, hooks.Payload{}, false, effNone},

		// ---- extra positives to round out 50+ & enrich combinations ----
		{"msgcount-notify-turnend2", "turn_end",
			`{ type: message_count, threshold: 10 }`, `{ type: notify, message: "very long" }`,
			schema.HookEventTurnEnd, hooks.Payload{MessageCount: 12}, true, effNone},
		{"toolcalls-log-turnend2", "turn_end",
			`{ type: tool_calls, threshold: 1 }`, `{ type: log, message: "used tools" }`,
			schema.HookEventTurnEnd, hooks.Payload{ToolCallsUsed: 3}, true, effNone},
		{"turncount-chain-turnend", "turn_end",
			`{ type: turn_count, threshold: 3 }`, `{ type: chain, actions: [ { type: log, message: "a" }, { type: notify, message: "b" } ] }`,
			schema.HookEventTurnEnd, hooks.Payload{TurnCount: 3}, true, effNone},
		{"anyof-content-toolend", "tool_end",
			`{ type: any_of, conditions: [ { type: content_contains, keyword: "fail" }, { type: tool_failed } ] }`,
			`{ type: log, level: warn, message: "maybe failing" }`,
			schema.HookEventToolEnd, hooks.Payload{LLMContent: "this will fail soon"}, true, effNone},
		{"allof-msgcount-turnend", "turn_end",
			`{ type: all_of, conditions: [ { type: always }, { type: message_count, threshold: 1 } ] }`,
			`{ type: log, message: "composite" }`,
			schema.HookEventTurnEnd, hooks.Payload{MessageCount: 5}, true, effNone},
		{"errortype-notify-error2", "error",
			`{ type: error_type, match: "Connection" }`, `{ type: notify, level: error, message: "net error" }`,
			schema.HookEventError, hooks.Payload{ErrorType: "ConnectionReset"}, true, effNone},
		{"toolname-transformparams-alias", "pre_tool_use",
			`{ type: tool_name, match: "filesystem.read" }`, `{ type: transform_params, transformation: { path: "expected.txt" } }`,
			schema.HookEventToolStart, hooks.Payload{ToolName: "filesystem.read", ToolArgs: map[string]any{"path": "decoy.txt"}}, true, effModified},
	}
}

// TestHooks_FiftyApps_CompileAndFire is the HK-8 guarantee. It proves,
// for 52 distinct apps, that following the documentation produces a hook
// that (a) compiles through the real compiler+catalog and (b) fires (or
// is correctly gated) through the real engine, with the documented side
// effect applied. Coverage of all conditions/actions/events is asserted
// at the end so the matrix can never silently shrink.
func TestHooks_FiftyApps_CompileAndFire(t *testing.T) {
	specs := fiftyHookSpecs()
	if len(specs) < 50 {
		t.Fatalf("need >= 50 apps, have %d", len(specs))
	}

	root := t.TempDir()
	comp := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})

	condSeen := map[string]bool{}
	actSeen := map[string]bool{}
	eventSeen := map[string]bool{}

	for _, sp := range specs {
		t.Run(sp.id, func(t *testing.T) {
			appID := "hk50-" + sp.id
			appDir := filepath.Join(root, appID)
			if err := os.MkdirAll(appDir, 0o755); err != nil {
				t.Fatal(err)
			}
			yaml := fiftyAppYAML(appID, sp.on, sp.cond, sp.act)
			if err := os.WriteFile(filepath.Join(appDir, "app.yaml"), []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			// (a) REAL compile path — zero error diagnostics.
			res, err := comp.Compile(appDir)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if res == nil || res.Definition == nil {
				t.Fatal("nil compile result")
			}
			if errs := res.Diagnostics.Errors(); len(errs) != 0 {
				for _, d := range errs {
					t.Errorf("diag [%s] %s", d.Code, d.Message)
				}
				t.Fatalf("app %q produced %d compile error(s)", appID, len(errs))
			}
			if res.Definition.Runtime == nil || len(res.Definition.Runtime.Hooks) != 1 {
				t.Fatalf("expected exactly 1 compiled hook, got %+v", res.Definition.Runtime)
			}
			compiled := res.Definition.Runtime.Hooks[0]

			condSeen[string(compiled.Condition.Type)] = true
			actSeen[string(compiled.Action.Type)] = true
			eventSeen[sp.on] = true

			// (b) REAL engine fire on the COMPILED hook.
			deps := hooks.ActionDeps{
				Logger: silentLogger{},
				Sink:   &fakeSink{},
				Caller: &fakeCaller{},
			}
			eng := hooks.New([]schema.Hook{compiled}, deps)
			eng.Async = false

			fr := eng.Fire(context.Background(), sp.fire, nil, sp.payload)

			got := eng.FireCount("h1")
			want := 0
			if sp.wantFire {
				want = 1
			}
			if got != want {
				t.Fatalf("FireCount=%d want %d (cond=%s act=%s on=%s)",
					got, want, compiled.Condition.Type, compiled.Action.Type, sp.on)
			}

			if sp.wantFire {
				assertEffect(t, sp.effect, fr)
			}
		})
	}

	// Exhaustiveness : the matrix must touch every documented vocabulary.
	for _, c := range schema.AllHookConditions {
		if !condSeen[string(c)] {
			t.Errorf("condition %q never exercised by the 50-app matrix", c)
		}
	}
	for _, a := range schema.AllHookActions {
		if !actSeen[string(a)] {
			t.Errorf("action %q never exercised by the 50-app matrix", a)
		}
	}
	for _, e := range schema.AllHookEvents {
		if _, notRouted := schema.NotYetRoutedHookEvents[e]; notRouted {
			continue // declared-only / not routed by design — cannot be exercised
		}
		if !eventSeen[string(e)] {
			t.Errorf("event %q never exercised by the 50-app matrix", e)
		}
	}
}

// assertEffect checks the engine applied the action's documented side
// effect (not merely incremented the counter).
func assertEffect(t *testing.T, kind effectKind, fr hooks.FireResult) {
	t.Helper()
	switch kind {
	case effGateBlock:
		if fr.Gate == nil || fr.Gate.Allow {
			t.Errorf("expected gate veto, got %+v", fr.Gate)
		}
	case effGateAllow:
		if fr.Gate == nil || !fr.Gate.Allow {
			t.Errorf("expected allow gate, got %+v", fr.Gate)
		}
	case effModified:
		if !fr.Modified {
			t.Error("expected Modified=true (transform_* did not apply)")
		}
	case effInject:
		if len(fr.Injects) == 0 || fr.Injects[0].Content == "" {
			t.Errorf("expected message injection, got %+v", fr.Injects)
		}
	}
}

// TestEngine_ConditionGatesBeforeBudget locks the ordering fix : a hook
// whose condition is FALSE must NOT consume its max_fires budget (nor
// reset its cooldown nor bump FireCount). The hook below fires only on a
// failed tool ; we send three SUCCESSFUL tool_end events (condition
// false) then one FAILED one (condition true). With max_fires=1 the only
// fire that may land is the failed one. Before the fix the three misses
// each burned the single allowance and the real failure was dropped.
func TestEngine_ConditionGatesBeforeBudget(t *testing.T) {
	hk := schema.Hook{
		ID:        "h1",
		On:        schema.HookEventToolEnd,
		MaxFires:  1,
		Condition: schema.HookCondition{Type: "tool_failed"},
		Action:    schema.HookAction{Type: "log", Params: map[string]any{"message": "tool failed"}},
	}
	eng := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentLogger{}})

	for i := 0; i < 3; i++ {
		eng.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{ToolStatus: "completed"})
	}
	if c := eng.FireCount("h1"); c != 0 {
		t.Fatalf("successful tools consumed budget : FireCount=%d, want 0", c)
	}
	eng.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{ToolStatus: "errored"})
	if c := eng.FireCount("h1"); c != 1 {
		t.Fatalf("the real failure did not fire (budget wrongly spent) : FireCount=%d, want 1", c)
	}
}
