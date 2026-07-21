package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	runtime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// The built-in task-completion stop guard must hold the turn open when the
// model tries to finish with an unfinished task : it vetoes the stop and
// injects a steering directive (naming the open task), then lets the loop
// run again. The hold is capped at maxStopVetoes (default 1) so a task that
// never completes can't wedge the loop — after one nudge the turn ends.
func TestStopHook_HoldsTurnThenCapEnds(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-stop")
	sess.state.Todos = []sessionstore.Todo{
		{ID: "t1", Text: "finish the migration", Status: "in_progress"},
		{ID: "t2", Text: "write the test", Status: "pending"},
	}
	// One scripted final answer ; every later round returns a synthetic
	// no-tool-call terminal, so each round re-triggers the stop guard.
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "All done!", Model: "gpt-4o-mini"},
	}}

	eng := hooks.New([]schema.Hook{hooks.TaskCompletionGuard}, hooks.ActionDeps{Logger: &e2eLogger{}})
	eng.Async = false

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-stop", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1 initial finish + 1 held continuation (cap=maxStopVetoes=1) = 2 calls.
	if lc.calls != 2 {
		t.Fatalf("expected 2 LLM calls (held once then capped), got %d", lc.calls)
	}
	// One hold injects one durable system directive.
	if got := sess.count(sessionstore.EventSystemMessage); got != 1 {
		t.Fatalf("expected 1 injected stop directive, got %d", got)
	}
	// The directive must name the open tasks via {{tasks.summary}}.
	ev := sess.find(sessionstore.EventSystemMessage)
	if ev == nil || ev.Message == nil {
		t.Fatal("no system directive event found")
	}
	if !strings.Contains(ev.Message.Content, "t1 (in_progress)") ||
		!strings.Contains(ev.Message.Content, "t2 (pending)") {
		t.Fatalf("directive did not name the open tasks: %q", ev.Message.Content)
	}
	// Must ride the authoritative <digitorn-directive> protocol so the model
	// treats it as binding, not a suggestion.
	if !strings.Contains(ev.Message.Content, "<digitorn-directive") ||
		!strings.Contains(ev.Message.Content, "</digitorn-directive>") {
		t.Fatalf("stop directive not wrapped in the authority envelope: %q", ev.Message.Content)
	}
}

// When the user redirects mid-work, the agent answers once, the stop guard
// nudges it (open tasks), and the agent — told it may stop — replies with an
// EMPTY message. That empty post-nudge reply is a deliberate silent stop: the
// turn ends with NO second assistant bubble (no duplicate content) and no
// further veto.
func TestStopHook_EmptyReplyAfterNudge_SilentStop(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-stop-empty")
	sess.state.Todos = []sessionstore.Todo{
		{ID: "t1", Text: "build the app", Status: "in_progress"},
	}
	// 1st reply: the real answer to the user. 2nd reply: EMPTY (agent read the
	// directive and chose to stop without repeating itself).
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "Sure, let's discuss.", Model: "gpt-4o-mini"},
		{Content: "", Model: "gpt-4o-mini"},
	}}

	eng := hooks.New([]schema.Hook{hooks.TaskCompletionGuard}, hooks.ActionDeps{Logger: &e2eLogger{}})
	eng.Async = false
	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-stop-empty", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two LLM calls (answer + empty), but only ONE visible assistant bubble.
	if lc.calls != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", lc.calls)
	}
	if got := sess.count(sessionstore.EventAssistantMessage); got != 1 {
		t.Fatalf("empty post-nudge reply must NOT create a second bubble; got %d assistant messages", got)
	}
}

// The task-completion guard is DISABLED by default: it is defined but not
// returned by BuiltinHooks(), so a turn ending with open tasks is NOT vetoed.
func TestStopHook_DisabledByDefault(t *testing.T) {
	for _, h := range hooks.BuiltinHooks() {
		if h.ID == hooks.BuiltinTaskCompletionGuardID {
			t.Fatal("task_completion_guard must be disabled (absent from BuiltinHooks)")
		}
	}
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-stop-disabled")
	sess.state.Todos = []sessionstore.Todo{
		{ID: "t1", Text: "still open", Status: "in_progress"},
	}
	lc := &stubLLM{responses: []*llm.ChatResponse{{Content: "Done.", Model: "gpt-4o-mini"}}}

	eng := hooks.New(hooks.BuiltinHooks(), hooks.ActionDeps{Logger: &e2eLogger{}})
	eng.Async = false
	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-stop-disabled", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.calls != 1 {
		t.Fatalf("disabled guard must not veto; expected 1 call, got %d", lc.calls)
	}
	if got := sess.count(sessionstore.EventSystemMessage); got != 0 {
		t.Fatalf("disabled guard must inject no directive, got %d", got)
	}
}

// With no open tasks the guard's condition (open_tasks > 0) is false : it
// never fires, the model's first finish stands, and the turn ends in one
// round. Proves the guard is inert for apps/sessions without a live plan.
func TestStopHook_NoOpenTasks_EndsImmediately(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-stop-clean")
	sess.state.Todos = []sessionstore.Todo{
		{ID: "t1", Text: "done thing", Status: "completed"},
	}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "Finished.", Model: "gpt-4o-mini"},
	}}

	eng := hooks.New([]schema.Hook{hooks.TaskCompletionGuard}, hooks.ActionDeps{Logger: &e2eLogger{}})
	eng.Async = false

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-stop-clean", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call (no veto), got %d", lc.calls)
	}
	if got := sess.count(sessionstore.EventSystemMessage); got != 0 {
		t.Fatalf("expected 0 stop directives, got %d", got)
	}
}

// runtime.max_stop_retries: 0 disables stop-hook holds entirely — even with
// open tasks the guard's veto is ignored and the turn ends in one round.
func TestStopHook_MaxStopRetriesZero_Disables(t *testing.T) {
	app := okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})
	zero := 0
	app.Definition.Runtime = &schema.RuntimeBlock{MaxStopRetries: &zero}
	apps := &stubApps{app: app}

	sess := newProjectingSessions("sess-stop-off")
	sess.state.Todos = []sessionstore.Todo{
		{ID: "t1", Text: "still open", Status: "in_progress"},
	}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "Done.", Model: "gpt-4o-mini"},
	}}

	eng := hooks.New([]schema.Hook{hooks.TaskCompletionGuard}, hooks.ActionDeps{Logger: &e2eLogger{}})
	eng.Async = false

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-stop-off", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.calls != 1 {
		t.Fatalf("max_stop_retries=0 should disable holds; expected 1 call, got %d", lc.calls)
	}
	if got := sess.count(sessionstore.EventSystemMessage); got != 0 {
		t.Fatalf("expected 0 stop directives when disabled, got %d", got)
	}
}
