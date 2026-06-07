package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// Shared helpers
// =====================================================================

// echoInner returns "name=...args=..." text part for any tool ; used
// to assert that run_parallel actually drove each call through the
// inner dispatcher.
type echoInner struct {
	mu    sync.Mutex
	calls []runtime.ToolInvocation
	err   error
}

func (e *echoInner) Dispatch(_ context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	e.mu.Lock()
	e.calls = append(e.calls, c)
	er := e.err
	e.mu.Unlock()
	if er != nil {
		return runtime.ToolOutcome{Status: "errored", Error: er.Error()}
	}
	raw, _ := json.Marshal(c.Args)
	return runtime.ToolOutcome{
		Status: "completed",
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: c.Name + ":" + string(raw)},
		},
	}
}

func decodeJSONOutcome(t *testing.T, o runtime.ToolOutcome) map[string]any {
	t.Helper()
	if o.Status != "completed" {
		t.Fatalf("status = %q, want completed (err=%q)", o.Status, o.Error)
	}
	if len(o.Parts) == 0 {
		t.Fatal("no parts")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(o.Parts[0].Text), &m); err != nil {
		t.Fatalf("decode: %v ; raw=%q", err, o.Parts[0].Text)
	}
	return m
}

// =====================================================================
// run_parallel
// =====================================================================

func TestRunParallel_FansOutAllCalls(t *testing.T) {
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner}

	actions := []any{
		map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "a"}},
		map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "b"}},
		map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "c"}},
	}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": actions},
	})
	body := decodeJSONOutcome(t, out)
	results, ok := body["results"].([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("results = %v", body["results"])
	}
	if got := len(inner.calls); got != 3 {
		t.Errorf("inner.calls = %d, want 3", got)
	}
}

func TestRunParallel_ResultsInputOrdered(t *testing.T) {
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner}

	actions := []any{
		map[string]any{"name": "x.a", "params": map[string]any{"i": float64(0)}},
		map[string]any{"name": "x.b", "params": map[string]any{"i": float64(1)}},
		map[string]any{"name": "x.c", "params": map[string]any{"i": float64(2)}},
	}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": actions},
	})
	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	for i, raw := range results {
		m := raw.(map[string]any)
		wantName := []string{"x.a", "x.b", "x.c"}[i]
		if m["name"] != wantName {
			t.Errorf("results[%d].name = %v, want %v", i, m["name"], wantName)
		}
	}
}

func TestRunParallel_EmptyCallsErrors(t *testing.T) {
	d := &meta.MetaDispatcher{}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{},
	})
	if out.Status != "errored" {
		t.Errorf("empty calls should error, got %q", out.Status)
	}
}

// A malformed action no longer aborts the whole batch — it is surfaced as
// one errored result while the valid actions still run (doc : "failures in
// one do not cancel the others"). The envelope itself completes.
func TestRunParallel_MalformedActionIsErroredResult(t *testing.T) {
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{
			"actions": []any{
				map[string]any{"name": "ok.tool", "params": map[string]any{}},
				"not a map",
			},
		},
	})
	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (one slot per input action)", len(results))
	}
	if results[0].(map[string]any)["status"] != "completed" {
		t.Errorf("valid action should complete : %v", results[0])
	}
	if results[1].(map[string]any)["status"] != "errored" {
		t.Errorf("malformed action should be errored : %v", results[1])
	}
}

func TestRunParallel_PartialFailureSurfaced(t *testing.T) {
	// First call succeeds, second errors. The results array must
	// contain both, in order.
	var idx atomic.Int32
	inner := &flakyInner{
		fn: func(c runtime.ToolInvocation) runtime.ToolOutcome {
			n := idx.Add(1)
			if n == 2 {
				return runtime.ToolOutcome{Status: "errored", Error: "boom"}
			}
			return runtime.ToolOutcome{
				Status: "completed",
				Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: "ok"},
				},
			}
		},
	}
	d := &meta.MetaDispatcher{Inner: inner}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": []any{
			map[string]any{"name": "a", "params": map[string]any{}},
			map[string]any{"name": "b", "params": map[string]any{}},
		}},
	})
	body := decodeJSONOutcome(t, out)
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d", len(results))
	}
}

func TestRunParallel_RejectsOver50Actions(t *testing.T) {
	d := &meta.MetaDispatcher{}
	bigList := make([]any, 51)
	for i := range bigList {
		bigList[i] = map[string]any{"name": "x.y", "params": map[string]any{}}
	}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": bigList},
	})
	if out.Status != "errored" {
		t.Errorf("51 actions should be rejected, got %q", out.Status)
	}
}

func TestRunParallel_Accepts50Actions(t *testing.T) {
	inner := &echoInner{}
	d := &meta.MetaDispatcher{Inner: inner}
	bigList := make([]any, 50)
	for i := range bigList {
		bigList[i] = map[string]any{"name": "x.y", "params": map[string]any{}}
	}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{"actions": bigList},
	})
	if out.Status != "completed" {
		t.Errorf("50 actions must be accepted, got %q (%s)", out.Status, out.Error)
	}
}

type flakyInner struct {
	fn func(runtime.ToolInvocation) runtime.ToolOutcome
}

func (f *flakyInner) Dispatch(_ context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	return f.fn(c)
}

// =====================================================================
// ask_user
// =====================================================================

type fakeAskUser struct {
	reply string
	err   error
	last  meta.AskUserRequest
}

func (f *fakeAskUser) Ask(_ context.Context, req meta.AskUserRequest) (string, error) {
	f.last = req
	return f.reply, f.err
}

func TestAskUser_RequiresQuestion(t *testing.T) {
	d := &meta.MetaDispatcher{AskUser: &fakeAskUser{}}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{},
	})
	if out.Status != "errored" {
		t.Errorf("missing question should error")
	}
}

func TestAskUser_NoBridgeFails(t *testing.T) {
	d := &meta.MetaDispatcher{}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{"question": "ok?"},
	})
	if out.Status != "errored" {
		t.Errorf("expected error with no bridge")
	}
}

func TestAskUser_ReturnsReply(t *testing.T) {
	br := &fakeAskUser{reply: "yes please"}
	d := &meta.MetaDispatcher{AskUser: br}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{"question": "continue?"},
	})
	body := decodeJSONOutcome(t, out)
	if body["raw_response"] != "yes please" || body["user_response"] != "yes please" {
		t.Errorf("response = %+v", body)
	}
	if br.last.Question != "continue?" {
		t.Errorf("bridge saw wrong question : %+v", br.last)
	}
}

func TestAskUser_MultiSelectFormatted(t *testing.T) {
	br := &fakeAskUser{reply: "Auth, Tests"}
	d := &meta.MetaDispatcher{AskUser: br}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{
			"question":       "Which features?",
			"choices":        []any{"Auth", "DB", "Tests"},
			"allow_multiple": true,
		},
	})
	body := decodeJSONOutcome(t, out)
	if body["user_response"] != "User selected: Auth, Tests" {
		t.Errorf("multi-select format = %v", body["user_response"])
	}
	if len(br.last.Choices) != 3 || !br.last.AllowMultiple {
		t.Errorf("bridge didn't receive choices/allow_multiple : %+v", br.last)
	}
}

func TestAskUser_FormParsedAndEditTracked(t *testing.T) {
	br := &fakeAskUser{reply: `{"framework":"FastAPI","name":"my-app"}`}
	d := &meta.MetaDispatcher{AskUser: br}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{
			"question": "Configure",
			"form":     []any{map[string]any{"type": "select", "name": "framework"}},
		},
	})
	body := decodeJSONOutcome(t, out)
	ur, _ := body["user_response"].(string)
	if !strings.Contains(ur, "User submitted form:") || !strings.Contains(ur, "framework: FastAPI") {
		t.Errorf("form not formatted : %v", ur)
	}
	if len(br.last.Form) != 1 {
		t.Errorf("bridge didn't receive form : %+v", br.last)
	}
}

func TestAskUser_AllowCustomDefaultsTrue(t *testing.T) {
	br := &fakeAskUser{reply: "x"}
	d := &meta.MetaDispatcher{AskUser: br}
	// No allow_custom arg with choices → the escape hatch is ON by default.
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{"question": "Pick", "choices": []any{"A", "B"}},
	})
	if !br.last.AllowCustom {
		t.Errorf("allow_custom should default to true on a proposal")
	}
	// Explicit false is respected (strict enum).
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{"question": "Pick", "choices": []any{"A"}, "allow_custom": false},
	})
	if br.last.AllowCustom {
		t.Errorf("allow_custom:false must be respected")
	}
}

func TestAskUser_RichFieldsReachBridge(t *testing.T) {
	br := &fakeAskUser{reply: "x"}
	d := &meta.MetaDispatcher{AskUser: br}
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{
			"question": "Name?", "default": "my-app", "placeholder": "type here",
			"multiline": true, "choices": []any{"A", "B"}, "allow_multiple": true,
			"min_select": float64(1), "max_select": float64(2),
		},
	})
	if br.last.Default != "my-app" || br.last.Placeholder != "type here" || !br.last.Multiline {
		t.Errorf("text-shape fields lost : %+v", br.last)
	}
	if br.last.MinSelect != 1 || br.last.MaxSelect != 2 {
		t.Errorf("min/max select lost : %+v", br.last)
	}
}

func TestAskUser_FormReplyTypeCoerced(t *testing.T) {
	// The user's client sent strings ; the daemon coerces to the declared types.
	br := &fakeAskUser{reply: `{"count":"5","enabled":"true","name":"x"}`}
	d := &meta.MetaDispatcher{AskUser: br}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{
			"question": "Configure",
			"form": []any{
				map[string]any{"name": "count", "type": "integer"},
				map[string]any{"name": "enabled", "type": "boolean"},
				map[string]any{"name": "name", "type": "text"},
			},
		},
	})
	body := decodeJSONOutcome(t, out)
	raw, _ := body["raw_response"].(string)
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("raw_response not JSON : %q", raw)
	}
	if n, _ := got["count"].(float64); n != 5 {
		t.Errorf("count not coerced to number : %v (%T)", got["count"], got["count"])
	}
	if b, _ := got["enabled"].(bool); !b {
		t.Errorf("enabled not coerced to bool : %v (%T)", got["enabled"], got["enabled"])
	}
	if got["name"] != "x" {
		t.Errorf("text field altered : %v", got["name"])
	}
}

func TestAskUser_FormNormalizedForWeakModels(t *testing.T) {
	br := &fakeAskUser{reply: "{}"}
	d := &meta.MetaDispatcher{AskUser: br}
	// A weak model writes a sloppy form : a field with no name, alias types
	// (int / dropdown / toggle), and options without a type. It must still work.
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{
			"question": "Configure",
			"form": []any{
				map[string]any{"label": "Worker Count", "type": "int"},
				map[string]any{"name": "db", "type": "dropdown", "options": []any{"pg", "mysql"}},
				map[string]any{"name": "feats", "options": []any{"a", "b"}},
				map[string]any{"name": "on", "type": "toggle"},
			},
		},
	})
	if len(br.last.Form) != 4 {
		t.Fatalf("form lost : %+v", br.last.Form)
	}
	if br.last.Form[0]["name"] != "worker_count" || br.last.Form[0]["type"] != "integer" {
		t.Errorf("nameless/int field not normalized : %+v", br.last.Form[0])
	}
	if br.last.Form[1]["type"] != "select" {
		t.Errorf("dropdown→select failed : %+v", br.last.Form[1])
	}
	if br.last.Form[2]["type"] != "select" {
		t.Errorf("options-without-type→select failed : %+v", br.last.Form[2])
	}
	if br.last.Form[3]["type"] != "boolean" {
		t.Errorf("toggle→boolean failed : %+v", br.last.Form[3])
	}
}

func TestAskUser_BridgeErrorSurfaced(t *testing.T) {
	br := &fakeAskUser{err: errors.New("timeout")}
	d := &meta.MetaDispatcher{AskUser: br}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.ask_user",
		Args: map[string]any{"question": "ok?"},
	})
	if out.Status != "errored" {
		t.Errorf("status = %q, want errored", out.Status)
	}
}

// =====================================================================
// background_run
// =====================================================================

type fakeBg struct {
	launchCalls  int
	statusCalls  int
	cancelled    []string
	listCalls    int
	statuses     map[string]meta.BackgroundStatus
	settleResult *meta.BackgroundStatus // non-nil → Wait returns it as "finished within settle"
}

func (f *fakeBg) Launch(_ context.Context, req meta.LaunchRequest) (string, error) {
	f.launchCalls++
	if f.statuses == nil {
		f.statuses = map[string]meta.BackgroundStatus{}
	}
	tid := req.Tool + "-id"
	f.statuses[tid] = meta.BackgroundStatus{TaskID: tid, Name: req.Tool, State: "running"}
	return tid, nil
}
func (f *fakeBg) Status(_ context.Context, _, taskID string) (meta.BackgroundStatus, error) {
	f.statusCalls++
	s, ok := f.statuses[taskID]
	if !ok {
		return meta.BackgroundStatus{}, errors.New("not found")
	}
	return s, nil
}
func (f *fakeBg) Wait(_ context.Context, _, taskID string, _ float64) (meta.BackgroundStatus, error) {
	if f.settleResult != nil {
		return *f.settleResult, nil // finished within the settle window
	}
	// Default: still running → the settle window times out (launch backgrounds it).
	return f.statuses[taskID], context.DeadlineExceeded
}
func (f *fakeBg) Cancel(_ context.Context, _, taskID string) error {
	f.cancelled = append(f.cancelled, taskID)
	return nil
}
func (f *fakeBg) List(_ context.Context, _ string) ([]meta.BackgroundStatus, error) {
	f.listCalls++
	out := make([]meta.BackgroundStatus, 0, len(f.statuses))
	for _, s := range f.statuses {
		out = append(out, s)
	}
	return out, nil
}

func TestBackground_Launch(t *testing.T) {
	bg := &fakeBg{}
	d := &meta.MetaDispatcher{Background: bg}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{"name": "filesystem.read", "params": map[string]any{}},
	})
	body := decodeJSONOutcome(t, out)
	if body["task_id"] != "filesystem.read-id" {
		t.Errorf("task_id wrong: %v", body)
	}
	if body["state"] != "running" {
		t.Errorf("a task still alive after the settle window must report running: %v", body)
	}
	if bg.launchCalls != 1 {
		t.Errorf("launchCalls = %d", bg.launchCalls)
	}
}

// TestBackground_SettleReturnsFastFailure : a task that FAILS inside the settle
// window (a server that crashes on a bad port) must return its error
// SYNCHRONOUSLY — the agent sees it immediately instead of a vague "running".
func TestBackground_SettleReturnsFastFailure(t *testing.T) {
	bg := &fakeBg{settleResult: &meta.BackgroundStatus{
		TaskID: "bash.run-id", Name: "bash.run", State: "errored",
		Error: "exit code 1", Result: "Error: listen EADDRINUSE :::3000",
	}}
	d := &meta.MetaDispatcher{Background: bg}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{"name": "bash.run", "params": map[string]any{"command": "node server.js"}},
	})
	body := decodeJSONOutcome(t, out)
	if body["state"] != "errored" {
		t.Fatalf("fast failure must return errored synchronously, got: %v", body)
	}
	if body["settled"] != true {
		t.Fatalf("result should be flagged settled: %v", body)
	}
	if body["error"] == nil || body["error"] == "" {
		t.Fatalf("the failure reason must be present: %v", body)
	}
}

func TestBackground_Status(t *testing.T) {
	bg := &fakeBg{statuses: map[string]meta.BackgroundStatus{
		"tid1": {TaskID: "tid1", Name: "x.y", State: "running"},
	}}
	d := &meta.MetaDispatcher{Background: bg}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{"task_id": "tid1"},
	})
	body := decodeJSONOutcome(t, out)
	if body["state"] != "running" {
		t.Errorf("state = %v", body)
	}
}

func TestBackground_Cancel(t *testing.T) {
	bg := &fakeBg{statuses: map[string]meta.BackgroundStatus{
		"tid1": {TaskID: "tid1", State: "running"},
	}}
	d := &meta.MetaDispatcher{Background: bg}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{"task_id": "tid1", "cancel": true},
	})
	if out.Status != "completed" {
		t.Errorf("cancel out: %+v", out)
	}
	if len(bg.cancelled) != 1 {
		t.Errorf("not cancelled : %v", bg.cancelled)
	}
}

func TestBackground_List(t *testing.T) {
	bg := &fakeBg{statuses: map[string]meta.BackgroundStatus{
		"a": {TaskID: "a"}, "b": {TaskID: "b"},
	}}
	d := &meta.MetaDispatcher{Background: bg}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{"list_tasks": true},
	})
	body := decodeJSONOutcome(t, out)
	tasks, ok := body["tasks"].([]any)
	if !ok || len(tasks) != 2 {
		t.Errorf("tasks = %v", body)
	}
}

func TestBackground_NoManagerFails(t *testing.T) {
	d := &meta.MetaDispatcher{}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.background_run",
		Args: map[string]any{"name": "x.y"},
	})
	if out.Status != "errored" {
		t.Errorf("no manager should error")
	}
}

// =====================================================================
// use_skill
// =====================================================================

type fakeSkillLoader struct {
	entry       meta.SkillEntry
	err         error
	lastApp     string
	lastUser    string
	lastCommand string
}

func (f *fakeSkillLoader) Load(_ context.Context, appID, userID, command string) (meta.SkillEntry, error) {
	f.lastApp = appID
	f.lastUser = userID
	f.lastCommand = command
	return f.entry, f.err
}

func TestUseSkill_ReturnsDocConformShape(t *testing.T) {
	ld := &fakeSkillLoader{entry: meta.SkillEntry{
		Command:     "/commit",
		Description: "Stage + commit",
		Content:     "# Commit workflow\n...",
	}}
	d := &meta.MetaDispatcher{SkillLoader: ld}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name:  "context_builder.use_skill",
		AppID: "my-app",
		Args:  map[string]any{"command": "/commit"},
	})
	body := decodeJSONOutcome(t, out)
	if body["success"] != true {
		t.Errorf("success flag missing : %v", body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not an object : %v", body)
	}
	if data["command"] != "/commit" {
		t.Errorf("command lost : %v", data)
	}
	if data["description"] != "Stage + commit" {
		t.Errorf("description lost : %v", data)
	}
	if data["content"] != ld.entry.Content {
		t.Errorf("content lost : %v", data)
	}
	if data["note"] != "Follow these instructions to complete the task." {
		t.Errorf("note missing or wrong : %v", data)
	}
	if ld.lastApp != "my-app" {
		t.Errorf("loader app id = %q", ld.lastApp)
	}
}

func TestUseSkill_AutoPrefixesSlash(t *testing.T) {
	ld := &fakeSkillLoader{entry: meta.SkillEntry{Command: "/commit", Content: "x"}}
	d := &meta.MetaDispatcher{SkillLoader: ld}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.use_skill",
		Args: map[string]any{"command": "commit"}, // no leading slash
	})
	if out.Status != "completed" {
		t.Fatalf("unexpected status %q (%s)", out.Status, out.Error)
	}
	if ld.lastCommand != "/commit" {
		t.Errorf("loader received %q, want /commit (auto-prefix)", ld.lastCommand)
	}
}

func TestUseSkill_MissingCommand(t *testing.T) {
	d := &meta.MetaDispatcher{SkillLoader: &fakeSkillLoader{}}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.use_skill",
		Args: map[string]any{},
	})
	if out.Status != "errored" {
		t.Errorf("missing command should error")
	}
}

func TestUseSkill_NoLoader(t *testing.T) {
	d := &meta.MetaDispatcher{}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.use_skill",
		Args: map[string]any{"command": "/x"},
	})
	if out.Status != "errored" {
		t.Errorf("no loader should error")
	}
}

func TestUseSkill_LoaderError(t *testing.T) {
	d := &meta.MetaDispatcher{SkillLoader: &fakeSkillLoader{err: errors.New("not found")}}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.use_skill",
		Args: map[string]any{"command": "/x"},
	})
	if out.Status != "errored" {
		t.Errorf("loader err should propagate")
	}
}

// =====================================================================
// call_app
// =====================================================================

type fakeAppCaller struct {
	reply      string
	err        error
	lastCaller string
	lastCalled string
	lastPrompt string
	lastUserID string
}

func (f *fakeAppCaller) Call(_ context.Context, callerID, calledID, prompt, userID string) (string, error) {
	f.lastCaller = callerID
	f.lastCalled = calledID
	f.lastPrompt = prompt
	f.lastUserID = userID
	return f.reply, f.err
}

func TestCallApp_Roundtrip(t *testing.T) {
	c := &fakeAppCaller{reply: "sub-app response"}
	d := &meta.MetaDispatcher{AppCaller: c}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name:   "context_builder.call_app",
		AppID:  "caller-app",
		UserID: "u1",
		Args: map[string]any{
			"app_id": "weather-bot",
			"prompt": "what's the weather in Paris?",
		},
	})
	body := decodeJSONOutcome(t, out)
	if body["reply"] != "sub-app response" {
		t.Errorf("reply lost : %v", body)
	}
	if c.lastCaller != "caller-app" || c.lastCalled != "weather-bot" {
		t.Errorf("caller args wrong : %+v", c)
	}
	if c.lastUserID != "u1" {
		t.Errorf("user id lost : %+v", c)
	}
}

func TestCallApp_MissingArgs(t *testing.T) {
	d := &meta.MetaDispatcher{AppCaller: &fakeAppCaller{}}
	for _, args := range []map[string]any{
		{}, {"app_id": "x"}, {"prompt": "y"},
	} {
		out := d.Dispatch(context.Background(), runtime.ToolInvocation{
			Name: "context_builder.call_app", Args: args,
		})
		if out.Status != "errored" {
			t.Errorf("args %v should error", args)
		}
	}
}

func TestCallApp_NoCaller(t *testing.T) {
	d := &meta.MetaDispatcher{}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.call_app",
		Args: map[string]any{"app_id": "x", "prompt": "y"},
	})
	if out.Status != "errored" {
		t.Errorf("no caller should error")
	}
}
