package runtime_test

import (
	"context"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// RT-4 — Engine integration : hooks fire at the canonical events.
// Doc reference : 31-tool-hooks.md.
// =====================================================================

// rt4Logger records every Info call so the test can assert which
// hooks ran.
type rt4Logger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *rt4Logger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}
func (l *rt4Logger) Warn(string, ...any)  {}
func (l *rt4Logger) Error(string, ...any) {}

func (l *rt4Logger) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// hookSourceWith serves a fixed engine for any appID and an empty
// per-agent slice. Doc-conform per the new HookSource interface.
type hookSourceWith struct {
	eng *hooks.Engine
}

func (s *hookSourceWith) ForApp(string) *hooks.Engine           { return s.eng }
func (s *hookSourceWith) ForAgent(string, string) []schema.Hook { return nil }

// makeLogHook builds a hook firing on `on` that emits a log entry
// with `marker` as its content.
func makeLogHook(id string, on schema.HookEvent, marker string) schema.Hook {
	return schema.Hook{
		ID:        id,
		On:        on,
		Condition: schema.HookCondition{Type: "always"},
		Action: schema.HookAction{
			Type:   "log",
			Params: map[string]any{"message": marker},
		},
	}
}

func TestRT4_TurnStartHookFires(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	logger := &rt4Logger{}
	eng := hooks.New(
		[]schema.Hook{makeLogHook("on-start", schema.HookEventTurnStart, "marker_start")},
		hooks.ActionDeps{Logger: logger},
	)
	eng.Async = false

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done"}}

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := logger.count("marker_start"); got != 1 {
		t.Errorf("turn_start fired %d times, want 1", got)
	}
}

func TestRT4_TurnEndHookFires(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	logger := &rt4Logger{}
	eng := hooks.New(
		[]schema.Hook{makeLogHook("on-end", schema.HookEventTurnEnd, "marker_end")},
		hooks.ActionDeps{Logger: logger},
	)
	eng.Async = false

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done"}}

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := logger.count("marker_end"); got != 1 {
		t.Errorf("turn_end fired %d times, want 1", got)
	}
}

func TestRT4_ToolStartEndHooksFireAroundDispatch(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	logger := &rt4Logger{}
	eng := hooks.New(
		[]schema.Hook{
			makeLogHook("pre", schema.HookEventToolStart, "marker_pre"),
			makeLogHook("post", schema.HookEventToolEnd, "marker_post"),
		},
		hooks.ActionDeps{Logger: logger},
	)
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "hello.txt"},
		}}},
		{Content: "Done."},
	}}

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})

	if got := logger.count("marker_pre"); got != 1 {
		t.Errorf("tool_start fired %d, want 1", got)
	}
	if got := logger.count("marker_post"); got != 1 {
		t.Errorf("tool_end fired %d, want 1", got)
	}
}

func TestRT4_PreToolUseAliasMatchesToolStart(t *testing.T) {
	// Hook declared with the alias pre_tool_use must fire on
	// canonical tool_start per 31-tool-hooks.md.
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	logger := &rt4Logger{}
	hk := schema.Hook{
		ID:        "alias",
		On:        schema.HookEventPreToolUse, // alias
		Condition: schema.HookCondition{Type: "always"},
		Action: schema.HookAction{
			Type: "log", Params: map[string]any{"message": "alias_fired"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "x.txt"},
		}}},
		{Content: "ok"},
	}}

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})

	if got := logger.count("alias_fired"); got != 1 {
		t.Errorf("alias hook fired %d times, want 1", got)
	}
}

func TestRT4_HookConditionFiltersToolName(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	logger := &rt4Logger{}
	hk := schema.Hook{
		ID: "shell-only",
		On: schema.HookEventToolStart,
		Condition: schema.HookCondition{
			Type:   "tool_name",
			Params: map[string]any{"match": "shell.bash"},
		},
		Action: schema.HookAction{
			Type: "log", Params: map[string]any{"message": "matched"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "x.txt"},
		}}},
		{Content: "Done."},
	}}

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})

	if got := logger.count("matched"); got != 0 {
		t.Errorf("hook fired %d times for non-matching tool, want 0", got)
	}
}

// TestRT4_GateVetoBlocksToolCall : a gate action with allow=false
// on tool_start must prevent the dispatcher from running. The
// outcome lands as "errored" with the gate reason.
func TestRT4_GateVetoBlocksToolCall(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	hk := schema.Hook{
		ID:        "block_read",
		On:        schema.HookEventToolStart,
		Condition: schema.HookCondition{Type: "tool_name", Params: map[string]any{"match": "filesystem.read"}},
		Action: schema.HookAction{
			Type:   "gate",
			Params: map[string]any{"allow": false, "reason": "blocked by policy"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentRT4Logger{}})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "hello.txt"},
		}}},
		{Content: "noted, refusing"},
	}}

	inner := &recordingInner{}

	cb := buildContextOnly(realDispatchActions())
	disp := buildMetaDispatcherWith(cb, inner)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})

	if inner.count != 0 {
		t.Errorf("dispatcher was reached %d times despite gate veto", inner.count)
	}
}

func TestRT4_NilHookSourceIsHarmless(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}

	e := newEngine(t, apps, sess, lc)
	e.Hooks = nil

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run with nil hooks: %v", err)
	}
}

type silentRT4Logger struct{}

func (silentRT4Logger) Info(string, ...any)  {}
func (silentRT4Logger) Warn(string, ...any)  {}
func (silentRT4Logger) Error(string, ...any) {}

// =====================================================================
// Effect application — proves the engine APPLIES hook effects, not just
// computes them. These lock the transform_result + inject_message gaps
// (the engine used to discard the FireResult for non-gate events).
// =====================================================================

// TestRT4_TransformResult_AppliedToOutcome : a transform_result hook on
// tool_end rewrites the tool result the agent sees. Before the fix the
// mutation hit a throwaway map and was lost.
func TestRT4_TransformResult_AppliedToOutcome(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-tr")

	hk := schema.Hook{
		ID:        "redact_result",
		On:        schema.HookEventToolEnd,
		Condition: schema.HookCondition{Type: "tool_name", Params: map[string]any{"match": "filesystem.read"}},
		Action: schema.HookAction{
			Type:   "transform_result",
			Params: map[string]any{"transformation": map[string]any{"text": "REDACTED-RESULT"}},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentRT4Logger{}})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read", Arguments: map[string]any{"path": "x.txt"},
		}}},
		{Content: "ok"},
	}}

	inner := &recordingInner{}
	cb := buildContextOnly(realDispatchActions())
	disp := buildMetaDispatcherWith(cb, inner)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-tr", UserID: "u",
	})

	ev := sess.find(sessionstore.EventToolResult)
	if ev == nil || ev.Tool == nil {
		t.Fatal("no tool_result event persisted")
	}
	found := false
	for _, p := range ev.Tool.Parts {
		if p.Type == sessionstore.PartTypeText && p.Text == "REDACTED-RESULT" {
			found = true
		}
	}
	if !found {
		t.Errorf("transform_result NOT applied to persisted outcome : parts=%+v", ev.Tool.Parts)
	}
}

// TestRT4_InjectMessage_PersistsMessage : an inject_message hook on
// turn_start must persist a real session message. Before the fix the
// Inject effect was discarded for non-gate events.
func TestRT4_InjectMessage_PersistsMessage(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-inj")

	hk := schema.Hook{
		ID:        "inject",
		On:        schema.HookEventTurnStart,
		Condition: schema.HookCondition{Type: "always"},
		Action: schema.HookAction{
			Type:   "inject_message",
			Params: map[string]any{"content": "INJECTED-MARKER", "role": "user"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentRT4Logger{}})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{{Content: "done"}}}

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-inj", UserID: "u",
	})

	var injected bool
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventUserMessage && ev.Message != nil &&
			ev.Message.Content == "INJECTED-MARKER" {
			injected = true
		}
	}
	if !injected {
		t.Error("inject_message hook did NOT persist a session message")
	}
}

// =====================================================================
// Root canonicalization — the cornerstone : when the LLM emits the
// underscored OpenAI wire form, hooks declared with the canonical
// dotted FQN MUST still match. Without canonicalization at the
// runtime boundary the entire hook engine becomes wire-format-
// sensitive, which is unacceptable for a feature documented as
// the daemon's JVM-equivalent.
// =====================================================================

// TestRT4_RootCanonicalization_HookMatchesUnderscoredWire : a hook
// declared with `tool_name match=filesystem.read` MUST fire when
// the LLM emits tool_call name="filesystem__read" (sanitized wire
// form). This is the core invariant.
func TestRT4_RootCanonicalization_HookMatchesUnderscoredWire(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-canon-1")

	logger := &rt4Logger{}
	hk := schema.Hook{
		ID: "match-dotted",
		On: schema.HookEventToolStart,
		Condition: schema.HookCondition{
			Type:   "tool_name",
			Params: map[string]any{"match": "filesystem.read"},
		},
		Action: schema.HookAction{
			Type: "log", Params: map[string]any{"message": "canonical_match"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	// LLM emits the UNDERSCORED form (wire-faithful simulation).
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "c1",
			Name:      "filesystem__read", // ← wire form
			Arguments: map[string]any{"path": "x.txt"},
		}}},
		{Content: "done"},
	}}

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-canon-1", UserID: "u",
	})

	if got := logger.count("canonical_match"); got != 1 {
		t.Errorf("hook fired %d times despite wire-form input ; canonicalization broken",
			got)
	}
}

// TestRT4_RootCanonicalization_TransformParamsAppliesOnUnderscoredCall :
// when the LLM emits the underscored form, a transform_params hook
// must still mutate the args before dispatch. Without canonicalization
// the condition would miss and the args would reach the dispatcher
// unmodified.
func TestRT4_RootCanonicalization_TransformParamsAppliesOnUnderscoredCall(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-canon-2")

	hk := schema.Hook{
		ID: "redirect",
		On: schema.HookEventToolStart,
		Condition: schema.HookCondition{
			Type:   "tool_name",
			Params: map[string]any{"match": "filesystem.read"},
		},
		Action: schema.HookAction{
			Type: "transform_params",
			Params: map[string]any{
				"transformation": map[string]any{"path": "expected.txt"},
			},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentRT4Logger{}})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "c1",
			Name:      "filesystem__read", // wire form
			Arguments: map[string]any{"path": "decoy.txt"},
		}}},
		{Content: "ok"},
	}}

	inner := &recordingInner{}
	cb := buildContextOnly(realDispatchActions())
	disp := buildMetaDispatcherWith(cb, inner)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-canon-2", UserID: "u",
	})

	if inner.count != 1 {
		t.Fatalf("inner dispatcher hit %d times, want 1", inner.count)
	}
	if got := inner.calls[0].Args["path"]; got != "expected.txt" {
		t.Errorf("transform_params did not mutate path : got %v (decoy=lost)", got)
	}
	if inner.calls[0].Name != "filesystem.read" {
		t.Errorf("inner saw %q, want canonical filesystem.read",
			inner.calls[0].Name)
	}
}

// TestRT4_RootCanonicalization_GateVetoesOnUnderscoredWire : a gate
// hook declared against the canonical FQN must veto a wire-form
// call. Tests the inverse of the match path.
func TestRT4_RootCanonicalization_GateVetoesOnUnderscoredWire(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-canon-3")

	hk := schema.Hook{
		ID:        "block",
		On:        schema.HookEventToolStart,
		Condition: schema.HookCondition{Type: "tool_name", Params: map[string]any{"match": "filesystem.read"}},
		Action:    schema.HookAction{Type: "gate", Params: map[string]any{"allow": false, "reason": "audit"}},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: silentRT4Logger{}})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "c1",
			Name:      "filesystem__read",
			Arguments: map[string]any{"path": "x.txt"},
		}}},
		{Content: "refused"},
	}}

	inner := &recordingInner{}
	cb := buildContextOnly(realDispatchActions())
	disp := buildMetaDispatcherWith(cb, inner)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, _ = e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-canon-3", UserID: "u",
	})

	if inner.count != 0 {
		t.Errorf("dispatcher reached %d times despite gate veto", inner.count)
	}
}
