package hooks_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// Fakes
// =====================================================================

type silentLogger struct{}

func (silentLogger) Info(string, ...any)  {}
func (silentLogger) Warn(string, ...any)  {}
func (silentLogger) Error(string, ...any) {}

type recordingLogger struct {
	mu     sync.Mutex
	infos  []string
	warns  []string
	errors []string
}

func (l *recordingLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}
func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *recordingLogger) Error(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, msg)
}

type fakeSink struct {
	mu     sync.Mutex
	events []sessionstore.Event
}

func (f *fakeSink) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return uint64(len(f.events)), nil
}

type fakeCaller struct {
	calls    atomic.Int64
	lastTool string
	lastArgs map[string]any
	result   string // text returned to the hook
	mu       sync.Mutex
}

func (f *fakeCaller) Call(_ context.Context, name string, args map[string]any) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastTool = name
	f.lastArgs = args
	res := f.result
	f.mu.Unlock()
	return res, nil
}

func newEngineSync(appHooks []schema.Hook, deps hooks.ActionDeps) *hooks.Engine {
	e := hooks.New(appHooks, deps)
	e.Async = false
	return e
}



func TestEvents_PreToolUseResolvesToToolStart(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID: "h1",
		On: schema.HookEventPreToolUse, // alias
		Action: schema.HookAction{
			Type:   "log",
			Params: map[string]any{"message": "fired", "level": "info"},
		},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventToolStart, nil, hooks.Payload{ToolName: "x"})
	if len(logger.infos) != 1 {
		t.Errorf("alias pre_tool_use should match canonical tool_start, got %d infos", len(logger.infos))
	}
}

func TestEvents_PostToolUseResolvesToToolEnd(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID:     "h1",
		On:     schema.HookEventPostToolUse,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "post"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{})
	if len(logger.infos) != 1 {
		t.Errorf("post_tool_use should match tool_end")
	}
}

func TestEvents_UserPromptResolvesToTurnStart(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID:     "h1",
		On:     schema.HookEventUserPrompt,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	if len(logger.infos) != 1 {
		t.Errorf("user_prompt should match turn_start")
	}
}



func TestCond_Always(t *testing.T) {
	if !hooks.EvalCondition(schema.HookCondition{Type: "always"}, hooks.Payload{}) {
		t.Error("always should pass")
	}
}
func TestCond_Never(t *testing.T) {
	if hooks.EvalCondition(schema.HookCondition{Type: "never"}, hooks.Payload{}) {
		t.Error("never should fail")
	}
}

func TestCond_ToolName_Glob(t *testing.T) {
	c := schema.HookCondition{Type: "tool_name", Params: map[string]any{"match": "filesystem.*"}}
	if !hooks.EvalCondition(c, hooks.Payload{ToolName: "filesystem.read"}) {
		t.Error("filesystem.* must match filesystem.read")
	}
	if hooks.EvalCondition(c, hooks.Payload{ToolName: "shell.bash"}) {
		t.Error("filesystem.* must not match shell.bash")
	}
}

func TestCond_ContextPressure(t *testing.T) {
	c := schema.HookCondition{Type: "context_pressure", Params: map[string]any{"threshold": 0.5}}
	if !hooks.EvalCondition(c, hooks.Payload{TokensUsed: 800, MaxTokens: 1000}) {
		t.Error("0.8 should exceed 0.5")
	}
	if hooks.EvalCondition(c, hooks.Payload{TokensUsed: 300, MaxTokens: 1000}) {
		t.Error("0.3 should not exceed 0.5")
	}
}

func TestCond_TurnCountEvery(t *testing.T) {
	c := schema.HookCondition{Type: "turn_count", Params: map[string]any{"threshold": 5, "every": 5}}
	for _, n := range []int{5, 10, 15, 20} {
		if !hooks.EvalCondition(c, hooks.Payload{TurnCount: n}) {
			t.Errorf("turn %d should fire (every 5, threshold 5)", n)
		}
	}
	for _, n := range []int{4, 6, 12} {
		if hooks.EvalCondition(c, hooks.Payload{TurnCount: n}) {
			t.Errorf("turn %d should NOT fire", n)
		}
	}
}

func TestCond_ContentContains(t *testing.T) {
	c := schema.HookCondition{Type: "content_contains", Params: map[string]any{"keyword": "boom"}}
	if !hooks.EvalCondition(c, hooks.Payload{LLMContent: "ka-boom! the build broke"}) {
		t.Error("keyword in LLM content should pass")
	}
	if !hooks.EvalCondition(c, hooks.Payload{UserMessage: "boom please"}) {
		t.Error("keyword in user message should pass")
	}
	if hooks.EvalCondition(c, hooks.Payload{LLMContent: "all good"}) {
		t.Error("no keyword should fail")
	}
}

func TestCond_ErrorType(t *testing.T) {
	c := schema.HookCondition{Type: "error_type", Params: map[string]any{"match": "Timeout"}}
	if !hooks.EvalCondition(c, hooks.Payload{ErrorType: "ConnectionTimeout"}) {
		t.Error("regex should match substring")
	}
}

func TestCond_Expression(t *testing.T) {
	c := schema.HookCondition{Type: "expression", Params: map[string]any{"expr": "tokens_used > 500"}}
	if !hooks.EvalCondition(c, hooks.Payload{TokensUsed: 600}) {
		t.Error("600 > 500 should be true")
	}
	if hooks.EvalCondition(c, hooks.Payload{TokensUsed: 100}) {
		t.Error("100 > 500 should be false")
	}
}

func TestCond_AllOf_AllPass(t *testing.T) {
	c := schema.HookCondition{
		Type: "all_of",
		Params: map[string]any{
			"conditions": []any{
				map[string]any{"type": "tool_name", "match": "filesystem.write"},
				map[string]any{"type": "always"},
			},
		},
	}
	if !hooks.EvalCondition(c, hooks.Payload{ToolName: "filesystem.write"}) {
		t.Error("both conditions met → all_of should pass")
	}
}

func TestCond_AnyOf(t *testing.T) {
	c := schema.HookCondition{
		Type: "any_of",
		Params: map[string]any{
			"conditions": []any{
				map[string]any{"type": "never"},
				map[string]any{"type": "tool_name", "match": "shell.bash"},
			},
		},
	}
	if !hooks.EvalCondition(c, hooks.Payload{ToolName: "shell.bash"}) {
		t.Error("any_of should pass when second matches")
	}
}

func TestCond_Not_SingleForm(t *testing.T) {
	c := schema.HookCondition{
		Type: "not",
		Params: map[string]any{
			"condition": map[string]any{"type": "tool_failed"},
		},
	}
	if !hooks.EvalCondition(c, hooks.Payload{ToolStatus: "completed"}) {
		t.Error("not(tool_failed) should pass when status=completed")
	}
	if hooks.EvalCondition(c, hooks.Payload{ToolStatus: "errored"}) {
		t.Error("not(tool_failed) should fail when status=errored")
	}
}

func TestCond_UnknownTypeIsFalse(t *testing.T) {
	if hooks.EvalCondition(schema.HookCondition{Type: "future_unknown"}, hooks.Payload{}) {
		t.Error("unknown condition should default to false")
	}
}



func TestAction_Log(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{
			"message": "{{tool.name}} fired", "level": "warn",
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{ToolName: "x.y"})
	if len(logger.warns) != 1 || logger.warns[0] != "x.y fired" {
		t.Errorf("templated log failed : %v", logger.warns)
	}
}

func TestAction_Notify(t *testing.T) {
	sink := &fakeSink{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventToolEnd,
		Action: schema.HookAction{Type: "notify", Params: map[string]any{
			"title": "alert", "message": "tool {{tool.name}} done", "level": "error",
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Sink: sink})
	e.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{
		SessionID: "s", ToolName: "filesystem.write",
	})
	if len(sink.events) != 1 {
		t.Fatalf("expected one event, got %d", len(sink.events))
	}
	if sink.events[0].Error == nil || sink.events[0].Error.Code != "error" {
		t.Errorf("level not stored : %+v", sink.events[0])
	}
}

func TestAction_InjectMessage_ReturnsInjection(t *testing.T) {
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "inject_message", Params: map[string]any{
			"content": "Reminder: {{tool.name}}", "role": "system",
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentLogger{}})
	res := e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{ToolName: "deploy"})
	if len(res.Injects) != 1 {
		t.Fatalf("expected one MessageInjection, got %d", len(res.Injects))
	}
	if res.Injects[0].Content != "Reminder: deploy" {
		t.Errorf("content lost : %q", res.Injects[0].Content)
	}
	if res.Injects[0].Role != "system" {
		t.Errorf("role lost : %q", res.Injects[0].Role)
	}
}


func TestEngine_MultipleInjectMessagesAllSurvive(t *testing.T) {
	mk := func(id, msg string, prio int) schema.Hook {
		return schema.Hook{
			ID: id, On: schema.HookEventTurnStart, Priority: prio,
			Action: schema.HookAction{Type: "inject_message", Params: map[string]any{
				"content": msg, "role": "user",
			}},
		}
	}
	// Declared high-then-low ; priority must order them low-then-high.
	e := newEngineSync(
		[]schema.Hook{mk("second", "MSG-SECOND", 200), mk("first", "MSG-FIRST", 50)},
		hooks.ActionDeps{Logger: silentLogger{}},
	)
	res := e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	if len(res.Injects) != 2 {
		t.Fatalf("expected BOTH injections, got %d : %+v", len(res.Injects), res.Injects)
	}
	if res.Injects[0].Content != "MSG-FIRST" || res.Injects[1].Content != "MSG-SECOND" {
		t.Errorf("inject order wrong : %q then %q", res.Injects[0].Content, res.Injects[1].Content)
	}
}


func TestAction_ChainWithTwoInjects(t *testing.T) {
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "chain", Params: map[string]any{
			"actions": []any{
				map[string]any{"type": "inject_message", "content": "CHAIN-A", "role": "user"},
				map[string]any{"type": "inject_message", "content": "CHAIN-B", "role": "user"},
			},
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentLogger{}})
	res := e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	if len(res.Injects) != 2 {
		t.Fatalf("chain should emit 2 injections, got %d", len(res.Injects))
	}
	if res.Injects[0].Content != "CHAIN-A" || res.Injects[1].Content != "CHAIN-B" {
		t.Errorf("chain inject order wrong : %+v", res.Injects)
	}
}



func TestAction_GateVetoesOnToolStart(t *testing.T) {
	hk := schema.Hook{
		ID: "block_rm", On: schema.HookEventToolStart,
		Condition: schema.HookCondition{Type: "tool_name", Params: map[string]any{"match": "shell.bash"}},
		Action:    schema.HookAction{Type: "gate", Params: map[string]any{"allow": false, "reason": "blocked"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentLogger{}})
	res := e.Fire(context.Background(), schema.HookEventToolStart, nil, hooks.Payload{ToolName: "shell.bash"})
	if res.Gate == nil || res.Gate.Allow {
		t.Errorf("expected gate veto, got %+v", res.Gate)
	}
}

func TestAction_GateAllowDoesNotBlock(t *testing.T) {
	hk := schema.Hook{
		ID: "always_allow", On: schema.HookEventToolStart,
		Action: schema.HookAction{Type: "gate", Params: map[string]any{"allow": true}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentLogger{}})
	res := e.Fire(context.Background(), schema.HookEventToolStart, nil, hooks.Payload{ToolName: "x"})
	if res.Gate == nil || !res.Gate.Allow {
		t.Errorf("expected allow gate, got %+v", res.Gate)
	}
}



func TestAction_TransformParams(t *testing.T) {
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventToolStart,
		Action: schema.HookAction{Type: "transform_params", Params: map[string]any{
			"transformation": map[string]any{"safety": "high"},
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentLogger{}})
	args := map[string]any{"path": "/tmp/x"}
	res := e.Fire(context.Background(), schema.HookEventToolStart, nil, hooks.Payload{
		ToolName: "x", ToolArgs: args,
	})
	if !res.Modified {
		t.Error("Modified flag should be set")
	}
	if args["safety"] != "high" {
		t.Errorf("transform_params didn't mutate args : %v", args)
	}
}


func TestAction_ModuleActionWithTemplating(t *testing.T) {
	caller := &fakeCaller{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventToolEnd,
		Action: schema.HookAction{Type: "module_action", Params: map[string]any{
			"module": "lsp",
			"action": "diagnose",
			"params": map[string]any{
				"file": "{{tool.params.path}}",
			},
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Caller: caller, Logger: silentLogger{}})
	e.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{
		ToolName: "filesystem.write",
		ToolArgs: map[string]any{"path": "/src/main.go"},
	})
	if caller.calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", caller.calls.Load())
	}
	if caller.lastTool != "lsp.diagnose" {
		t.Errorf("tool = %q", caller.lastTool)
	}
	if caller.lastArgs["file"] != "/src/main.go" {
		t.Errorf("template not rendered : %v", caller.lastArgs)
	}
}


type panicCompactor struct{}

func (panicCompactor) CompactSession(context.Context, string, string, int) error {
	panic("boom: compactor exploded")
}


func TestEngine_SyncActionPanic_DoesNotCrash(t *testing.T) {
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnEnd,
		Action: schema.HookAction{Type: "compact_context", Params: map[string]any{
			"strategy": "summarize", "keep_last": 10,
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{
		Logger: silentLogger{}, Compactor: panicCompactor{},
	})

	// Must NOT panic.
	_ = e.Fire(context.Background(), schema.HookEventTurnEnd, nil, hooks.Payload{SessionID: "s"})

	if got := e.FireCount("h1"); got != 1 {
		t.Errorf("FireCount = %d, want 1 (a contained panic must still count as a fire)", got)
	}
}


type panicLogger struct{}

func (panicLogger) Info(string, ...any)  { panic("boom: logger exploded") }
func (panicLogger) Warn(string, ...any)  {}
func (panicLogger) Error(string, ...any) {}


func TestEngine_AsyncActionPanic_DoesNotCrash(t *testing.T) {
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: panicLogger{}})

	// Must NOT panic (runHookAsync recovers, synchronously since Async=false).
	_ = e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})

	if got := e.FireCount("h1"); got != 1 {
		t.Errorf("FireCount = %d, want 1", got)
	}
}


func TestEngine_GateActionPanic_FailsOpen(t *testing.T) {

	hk := schema.Hook{
		ID: "h1", On: schema.HookEventToolStart,
		Action: schema.HookAction{Type: "compact_context", Params: map[string]any{"strategy": "summarize"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{
		Logger: silentLogger{}, Compactor: panicCompactor{},
	})
	res := e.Fire(context.Background(), schema.HookEventToolStart, nil, hooks.Payload{ToolName: "x"})
	if res.Gate != nil && !res.Gate.Allow {
		t.Error("a panicking sync action must not produce a veto (fail-open)")
	}
}



func TestEngine_Cooldown(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Cooldown: 5,
		Action:   schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})

	// 3 fires within 1 second : only the first should land.
	for i := 0; i < 3; i++ {
		e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	}
	if len(logger.infos) != 1 {
		t.Errorf("cooldown not enforced : got %d infos, want 1", len(logger.infos))
	}
	if e.FireCount("h1") != 1 {
		t.Errorf("FireCount = %d, want 1", e.FireCount("h1"))
	}
}



func TestEngine_PriorityOrdering(t *testing.T) {
	logger := &recordingLogger{}
	hk1 := schema.Hook{
		ID: "low", On: schema.HookEventTurnStart, Priority: 50,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "low"}},
	}
	hk2 := schema.Hook{
		ID: "high", On: schema.HookEventTurnStart, Priority: 200,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "high"}},
	}
	// Declared in "high then low" order ; priority should reverse them.
	e := newEngineSync([]schema.Hook{hk2, hk1}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	if len(logger.infos) != 2 {
		t.Fatalf("want 2 infos, got %d", len(logger.infos))
	}
	if logger.infos[0] != "low" || logger.infos[1] != "high" {
		t.Errorf("priority order broken : %v", logger.infos)
	}
}



func TestEngine_PerAgentHooksMerged(t *testing.T) {
	logger := &recordingLogger{}
	appHk := schema.Hook{
		ID: "app", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "app"}},
	}
	agentHk := schema.Hook{
		ID: "agent", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "agent"}},
	}
	e := newEngineSync([]schema.Hook{appHk}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventTurnStart, []schema.Hook{agentHk}, hooks.Payload{})
	if len(logger.infos) != 2 {
		t.Errorf("expected app + agent fires, got %v", logger.infos)
	}
}



func TestEngine_DisabledSkips(t *testing.T) {
	logger := &recordingLogger{}
	disabled := false
	hk := schema.Hook{
		ID: "off", On: schema.HookEventTurnStart, Enabled: &disabled,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	if len(logger.infos) != 0 {
		t.Errorf("disabled hook fired")
	}
}



func TestEngine_CancelledCtxSkips(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.Fire(ctx, schema.HookEventTurnStart, nil, hooks.Payload{})
	if len(logger.infos) != 0 {
		t.Errorf("cancelled ctx fired anyway")
	}
}



func TestEngine_NilEngineSafe(t *testing.T) {
	var e *hooks.Engine
	res := e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	if res.Gate != nil || len(res.Injects) != 0 {
		t.Error("nil engine should produce empty result")
	}
}



func TestEngine_MaxFires(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		MaxFires: 2,
		Action:   schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	for i := 0; i < 10; i++ {
		e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
	}
	if e.FireCount("h1") != 2 {
		t.Errorf("max_fires not enforced : %d", e.FireCount("h1"))
	}
}

// =====================================================================
// 14. Concurrent fires safe
// =====================================================================

func TestEngine_ConcurrentFires(t *testing.T) {
	logger := &recordingLogger{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
		}()
	}
	wg.Wait()
	if e.FireCount("h1") != N {
		t.Errorf("concurrent fires = %d, want %d", e.FireCount("h1"), N)
	}
}

// =====================================================================
// 15. Templating helpers
// =====================================================================

func TestTemplating_ToolPathsResolveCorrectly(t *testing.T) {
	caller := &fakeCaller{}
	hk := schema.Hook{
		ID: "h1", On: schema.HookEventToolEnd,
		Action: schema.HookAction{Type: "module_action", Params: map[string]any{
			"action": "x.y",
			"params": map[string]any{
				"who":     "{{tool.name}}",
				"why":     "error={{tool.error}}",
				"params0": "{{tool.params.list.0}}",
				"nested":  "{{tool.result.status}}",
			},
		}},
	}
	e := newEngineSync([]schema.Hook{hk}, hooks.ActionDeps{Caller: caller, Logger: silentLogger{}})
	e.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{
		ToolName:   "filesystem.write",
		ToolError:  "ENOENT",
		ToolArgs:   map[string]any{"list": []any{"first", "second"}},
		ToolResult: map[string]any{"status": "errored"},
	})
	args := caller.lastArgs
	if args["who"] != "filesystem.write" {
		t.Errorf("name resolution: %v", args["who"])
	}
	if args["why"] != "error=ENOENT" {
		t.Errorf("error resolution: %v", args["why"])
	}
	if args["params0"] != "first" {
		t.Errorf("array resolution: %v", args["params0"])
	}
	if args["nested"] != "errored" {
		t.Errorf("result resolution: %v", args["nested"])
	}
}



func TestEngine_AsyncReturnsBeforeActionFinishes(t *testing.T) {
	released := make(chan struct{})
	wait := make(chan struct{})
	hk := schema.Hook{
		ID: "slow", On: schema.HookEventTurnStart,
		Action: schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}
	logger := &gatedLogger{wait: wait, released: released}
	e := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	e.Async = true

	done := make(chan struct{})
	go func() {
		e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
		close(done)
	}()
	select {
	case <-done:
		// good : Fire returned without waiting on the logger
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Fire did not return promptly in async mode")
	}
	close(released) // release the action so the goroutine exits cleanly
	<-wait
}

type gatedLogger struct {
	wait     chan<- struct{}
	released <-chan struct{}
}

func (g *gatedLogger) Info(string, ...any) {
	<-g.released
	g.wait <- struct{}{}
}
func (g *gatedLogger) Warn(string, ...any)  {}
func (g *gatedLogger) Error(string, ...any) {}
