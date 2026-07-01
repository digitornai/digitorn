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
// run again. The hold is capped at maxStopVetoes so a task that never
// completes can't wedge the loop — after the cap the turn ends.
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

	eng := hooks.New(hooks.BuiltinHooks(), hooks.ActionDeps{Logger: &e2eLogger{}})
	eng.Async = false

	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-stop", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1 initial finish + 2 held continuations (cap=maxStopVetoes=2) = 3 calls.
	if lc.calls != 3 {
		t.Fatalf("expected 3 LLM calls (held twice then capped), got %d", lc.calls)
	}
	// Each hold injects a durable system directive ; expect 2.
	if got := sess.count(sessionstore.EventSystemMessage); got != 2 {
		t.Fatalf("expected 2 injected stop directives, got %d", got)
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

	eng := hooks.New(hooks.BuiltinHooks(), hooks.ActionDeps{Logger: &e2eLogger{}})
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

	eng := hooks.New(hooks.BuiltinHooks(), hooks.ActionDeps{Logger: &e2eLogger{}})
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
