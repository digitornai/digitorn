package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/toolname"
)

type AskUserBridge interface {
	Ask(ctx context.Context, req AskUserRequest) (string, error)
}

type AskUserRequest struct {
	SessionID     string
	AppID         string
	UserID        string
	TurnID        string
	Question      string
	Content       string
	ResponseType  string
	Choices       []string
	AllowMultiple bool
	AllowCustom   bool
	MinSelect     int
	MaxSelect     int
	Default       string
	Placeholder   string
	Multiline     bool
	Form          []map[string]any
	TimeoutSecs   float64
}

type SkillLoader interface {
	Load(ctx context.Context, appID, userID, command string) (SkillEntry, error)
}

type SkillEntry struct {
	Command     string
	Description string
	Content     string
	// Dir is the absolute directory the skill file lives in, when the skill
	// comes from an app bundle. Handed to the agent so a SKILL.md can point at
	// its OWN sibling files and scripts ("run scripts/validate.py") and have
	// them actually resolve — the progressive-disclosure level where a skill
	// stops being a text blob and becomes a packaged capability. Empty for
	// user-authored skills, which are a single stored text field.
	Dir string
}

type AppCaller interface {
	Call(ctx context.Context, callerAppID, calledAppID, prompt, userID string) (string, error)
}

type BackgroundManager interface {
	Launch(ctx context.Context, req LaunchRequest) (taskID string, err error)
	Status(ctx context.Context, sessionID, taskID string) (BackgroundStatus, error)
	Wait(ctx context.Context, sessionID, taskID string, timeoutSecs float64) (BackgroundStatus, error)
	Cancel(ctx context.Context, sessionID, taskID string) error
	List(ctx context.Context, sessionID string) ([]BackgroundStatus, error)
}

type LaunchRequest struct {
	SessionID  string
	AppID      string
	UserID     string
	AgentID    string
	Tool       string
	Args       map[string]any
	NotifyWhen string
}

type BackgroundStatus struct {
	TaskID    string `json:"task_id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt int64  `json:"started_at_unix,omitempty"`
	Log string `json:"log,omitempty"`
}

const runParallelMaxActions = 256

type ctxKey int

const parallelDepthKey ctxKey = iota

const maxParallelDepth = 1

func parallelDepth(ctx context.Context) int {
	d, _ := ctx.Value(parallelDepthKey).(int)
	return d
}

var parallelWrapperKeys = []string{"tasks", "actions", "calls", "tools", "invocations", "steps", "items", llm.ArgsArrayKey}

func extractParallelActions(args map[string]any) []any {
	if args == nil {
		return nil
	}
	for _, k := range parallelWrapperKeys {
		if arr, ok := asActionArray(args[k]); ok {
			return arr
		}
	}
	for _, v := range args {
		if arr, ok := asActionArray(v); ok {
			if obj, ok := arr[0].(map[string]any); ok && parallelToolName(obj) != "" {
				return arr
			}
		}
	}
	if len(args) == 1 {
		for _, v := range args {
			if arr, ok := asActionArray(v); ok {
				return arr
			}
		}
	}
	return nil
}

func asActionArray(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, len(t) > 0
	case string:
		s := strings.TrimSpace(t)
		if strings.HasPrefix(s, "[") {
			var arr []any
			if json.Unmarshal([]byte(s), &arr) == nil && len(arr) > 0 {
				return arr, true
			}
		}
	}
	return nil, false
}

func parallelToolName(obj map[string]any) string {
	for _, k := range []string{"tool", "name", "action", "tool_name"} {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func parallelArgs(obj map[string]any) any {
	for _, k := range []string{"args", "params", "arguments", "input", "parameters"} {
		if v, ok := obj[k]; ok {
			return v
		}
	}
	return nil
}

func (m *MetaDispatcher) handleRunParallel(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if parallelDepth(ctx) >= maxParallelDepth {
		return errored("run_parallel: nested run_parallel is not allowed; flatten the actions into a single call")
	}

	raw := extractParallelActions(call.Args)
	if len(raw) == 0 {
		return errored(`run_parallel: provide a non-empty list of {tool, args}, e.g. {"tasks":[{"tool":"filesystem.read","args":{"path":"a.go"}}]} (a bare array also works)`)
	}
	if len(raw) > runParallelMaxActions {
		return errored(fmt.Sprintf("run_parallel: too many actions (%d > %d)", len(raw), runParallelMaxActions))
	}

	n := len(raw)
	names := make([]string, n)
	outcomes := make([]runtime.ToolOutcome, n)
	filled := make([]bool, n)

	type childResult struct {
		idx     int
		outcome runtime.ToolOutcome
	}
	resCh := make(chan childResult, n)

	childCtx := context.WithValue(ctx, parallelDepthKey, parallelDepth(ctx)+1)

	launched := 0
	for i := range raw {
		obj, ok := raw[i].(map[string]any)
		if !ok {
			outcomes[i] = errored(fmt.Sprintf("run_parallel: item %d is not an object — expected {tool, args}", i))
			filled[i] = true
			continue
		}
		name := parallelToolName(obj)
		names[i] = name
		if name == "" {
			outcomes[i] = errored(fmt.Sprintf("run_parallel: item %d must name a tool (key \"tool\")", i))
			filled[i] = true
			continue
		}
		params, perr := coerceParams(parallelArgs(obj))
		if perr != nil {
			outcomes[i] = errored(fmt.Sprintf("run_parallel: item %d `args` is not valid JSON — %s", i, perr.Error()))
			filled[i] = true
			continue
		}
		launched++
		go func(i int, name string, params map[string]any) {
			// Panic isolation : a sub-tool that panics must surface as one
			// errored result, never crash the daemon or cancel its siblings.
			// An unrecovered panic in any goroutine takes the whole process
			// down, so every child runs under its own recover.
			defer func() {
				if r := recover(); r != nil {
					if m.Logger != nil {
						m.Logger.Error("run_parallel: action panicked",
							slog.String("action", name),
							slog.Any("panic", r),
							slog.String("stack", string(debug.Stack())))
					}
					resCh <- childResult{i, runtime.ToolOutcome{
						Status: "errored",
						Error:  fmt.Sprintf("run_parallel: action %q panicked: %v", name, r),
					}}
				}
			}()
			child := runtime.ToolInvocation{
				CallID: fmt.Sprintf("%s:%d", call.CallID, i),
				Name:       ResolveAlias(Canonicalize(name)),
				Args:       params,
				AppID:      call.AppID,
				AgentID:    call.AgentID,
				UserID:     call.UserID,
				SessionID:  call.SessionID,
				AgentRunID: call.AgentRunID,
				UserJWT:    call.UserJWT,
			}
			// UNIVERSAL bare-action recovery : qualify bare names (e.g. "read"
			// → "filesystem.read") so the gate 1a module check sees the correct
			// module. Without this, splitToolName("read") returns module="",
			// and CanAgentCall("", "read") fails → denied even though the
			// identical direct call succeeds.
			if !strings.Contains(child.Name, ".") {
				if idx := m.resolveIndex(call); idx != nil {
					fqns := idx.FQNList()
					child.Name = toolname.QualifyBareName(child.Name, fqns)
					if !strings.Contains(child.Name, ".") {
						child.Name = toolname.ResolveMangled(child.Name, fqns)
					}
				}
			}
			// SG-4 chokepoint : gate each fanned-out child before it runs.
			if blocked := m.gateTarget(childCtx, child); blocked != nil {
				resCh <- childResult{i, *blocked}
				return
			}
			resCh <- childResult{i, m.Dispatch(childCtx, child)}
		}(i, name, params)
	}

	for got := 0; got < launched; {
		select {
		case r := <-resCh:
			outcomes[r.idx] = r.outcome
			filled[r.idx] = true
			got++
			if m.Progress != nil {
				m.Progress(ctx, sessionstore.Event{
					Type:          sessionstore.EventToolProgress,
					SessionID:     call.SessionID,
					AppID:         call.AppID,
					UserID:        call.UserID,
					CorrelationID: call.CallID,
					Tool: &sessionstore.ToolPayload{
						CallID:   fmt.Sprintf("%s:%d", call.CallID, r.idx),
						Name:     ResolveAlias(Canonicalize(names[r.idx])),
						Status:   r.outcome.Status,
						Error:    r.outcome.Error,
						Metadata: map[string]any{"index": r.idx, "total": n, "completed": got},
					},
				})
			}
		case <-childCtx.Done():
			for j := range outcomes {
				if !filled[j] {
					outcomes[j] = runtime.ToolOutcome{
						Status: "errored",
						Error:  "run_parallel: cancelled before completion: " + childCtx.Err().Error(),
					}
					filled[j] = true
				}
			}
			got = launched
		}
	}

	results := make([]map[string]any, n)
	for i := range outcomes {
		results[i] = map[string]any{
			"name":   names[i],
			"status": outcomes[i].Status,
		}
		if outcomes[i].Error != "" {
			results[i]["error"] = outcomes[i].Error
		}
		var content string
		for _, p := range outcomes[i].Parts {
			content += p.Text
		}
		if content != "" {
			results[i]["content"] = content
		}
	}
	return jsonOutcome(map[string]any{"results": results})
}

// handleAskUser : pause the loop and ask the user a typed question.
// Returns the reply text when AskUser bridge is wired ; otherwise
// errors with "ask_user not wired" so the LLM sees the failure.
func (m *MetaDispatcher) handleAskUser(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	question, _ := call.Args["question"].(string)
	if question == "" {
		return errored("ask_user: 'question' is required")
	}
	if m.AskUser == nil {
		return errored("ask_user not wired (no AskUserBridge)")
	}
	content, _ := call.Args["content"].(string)
	// Agent-declared reply shape. Only "approval" (Approve/Reject verdict on
	// content) diverges from the default answer flow; anything else is "answer".
	responseType, _ := call.Args["response_type"].(string)
	responseType = strings.ToLower(strings.TrimSpace(responseType))
	if responseType != "approval" {
		responseType = "answer"
	}
	timeout, _ := call.Args["timeout"].(float64)
	choices := stringSliceArg(call.Args["choices"])
	allowMultiple := boolArg(call.Args["allow_multiple"])
	// Normalize a model-written form so even a weak model produces a working one :
	// every field gets a name + a canonical type (aliases folded, type inferred
	// from options).
	form := normalizeAskUserForm(mapSliceArg(call.Args["form"]))
	// The custom-answer escape hatch is ON by default whenever the agent makes
	// proposals — the user can always answer something the agent didn't foresee.
	// The agent opts out only for a strict enum (allow_custom: false).
	allowCustom := true
	if _, ok := call.Args["allow_custom"]; ok {
		allowCustom = boolArg(call.Args["allow_custom"])
	}
	defaultAns, _ := call.Args["default"].(string)
	placeholder, _ := call.Args["placeholder"].(string)
	multiline := boolArg(call.Args["multiline"])
	minSelect, _ := call.Args["min_select"].(float64)
	maxSelect, _ := call.Args["max_select"].(float64)

	req := AskUserRequest{
		SessionID:     call.SessionID,
		AppID:         call.AppID,
		UserID:        call.UserID,
		TurnID:        call.AgentRunID,
		Question:      question,
		Content:       content,
		ResponseType:  responseType,
		Choices:       choices,
		AllowMultiple: allowMultiple,
		AllowCustom:   allowCustom,
		MinSelect:     int(minSelect),
		MaxSelect:     int(maxSelect),
		Default:       defaultAns,
		Placeholder:   placeholder,
		Multiline:     multiline,
		Form:          form,
		TimeoutSecs:   timeout,
	}
	reply, err := m.AskUser.Ask(ctx, req)
	if err != nil {
		return errored("ask_user: " + err.Error())
	}

	// Rich response (doc-conform) : the raw reply plus a human-readable
	// formatting, and — when content was offered — whether the user
	// edited it. A form reply is the field JSON ; a multi-select reply
	// is comma-separated choices.
	out := map[string]any{
		"status":        "answered",
		"question":      question,
		"raw_response":  coerceAskUserReply(req, reply),
		"user_response": formatAskUserResponse(req, reply),
	}
	if content != "" {
		out["content_was_edited"] = reply != "" && strings.TrimSpace(reply) != strings.TrimSpace(content)
		if reply != "" {
			out["edited_content"] = reply
		}
	}
	return jsonOutcome(out)
}

// formatAskUserResponse renders the raw user reply into a readable form
// based on the question shape : a form's JSON object becomes a labelled
// list, a multi-select becomes "User selected: a, b", everything else
// passes through. Mirrors the reference daemon's _format_ask_user_response.
func formatAskUserResponse(req AskUserRequest, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(req.Form) > 0 {
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err == nil && len(fields) > 0 {
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var b strings.Builder
			b.WriteString("User submitted form:")
			for _, k := range keys {
				fmt.Fprintf(&b, "\n  - %s: %v", k, fields[k])
			}
			return b.String()
		}
	}
	if len(req.Choices) > 0 && req.AllowMultiple {
		parts := strings.Split(raw, ",")
		picked := parts[:0]
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				picked = append(picked, s)
			}
		}
		if len(picked) > 1 {
			return "User selected: " + strings.Join(picked, ", ")
		}
	}
	return raw
}

// coerceAskUserReply retypes a form reply against its field declarations so the
// agent receives typed values (the UI validates ; the daemon coerces). A numeric
// field's value becomes a number, a boolean's becomes a bool — even if the client
// sent a string. Non-form replies pass through untouched.
func coerceAskUserReply(req AskUserRequest, raw string) string {
	if len(req.Form) == 0 {
		return raw
	}
	var vals map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &vals); err != nil || len(vals) == 0 {
		return raw
	}
	for _, f := range req.Form {
		name, _ := f["name"].(string)
		v, ok := vals[name]
		if name == "" || !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(f["type"]))) {
		case "number", "float", "range", "rating":
			if n, ok := toFloat(v); ok {
				vals[name] = n
			}
		case "int", "integer":
			if n, ok := toFloat(v); ok {
				vals[name] = int64(n)
			}
		case "boolean", "bool", "confirm", "toggle":
			vals[name] = toBool(v)
		}
	}
	if b, err := json.Marshal(vals); err == nil {
		return string(b)
	}
	return raw
}

// toFloat coerces a JSON number or numeric string to float64.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// toBool coerces a JSON bool or truthy string (true/yes/1/on) to bool.
func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "1", "on", "y":
			return true
		}
	}
	return false
}

// normalizeAskUserForm makes a model-written form robust so even a weak model
// produces a working one : every field gets a non-empty `name` (slug of label,
// else fieldN) and a canonical `type` (aliases folded; a field with options but
// no usable type becomes a select). Mutates the maps in place and returns them.
func normalizeAskUserForm(form []map[string]any) []map[string]any {
	for i, f := range form {
		if f == nil {
			continue
		}
		name, _ := f["name"].(string)
		if strings.TrimSpace(name) == "" {
			if lbl, _ := f["label"].(string); strings.TrimSpace(lbl) != "" {
				name = slugify(lbl)
			}
			if name == "" {
				name = fmt.Sprintf("field%d", i+1)
			}
			f["name"] = name
		}
		t, _ := f["type"].(string)
		canon := canonFieldType(t)
		if canon == "" {
			if _, ok := f["options"]; ok {
				canon = "select"
			}
		}
		if canon != "" {
			f["type"] = canon
		}
	}
	return form
}

// canonFieldType folds the aliases a model is likely to write into the canonical
// field type. An unrecognised value returns "" so the caller can fall back to
// options-inference (→ select) or free-text.
func canonFieldType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "text", "string", "str", "input", "line":
		return "text"
	case "textarea", "multiline", "text_area", "long_text", "longtext", "paragraph":
		return "textarea"
	case "number", "float", "double", "decimal":
		return "number"
	case "int", "integer":
		return "integer"
	case "bool", "boolean", "toggle", "confirm", "yesno", "yes_no":
		return "boolean"
	case "select", "choice", "dropdown", "enum", "picklist", "radio", "single":
		return "select"
	case "multiselect", "multi_select", "multi-select", "multichoice", "checkboxes", "tags", "multi":
		return "multiselect"
	case "date", "datetime", "time", "day":
		return "date"
	case "email", "mail":
		return "email"
	case "url", "link", "uri":
		return "url"
	case "password", "secret", "masked":
		return "password"
	case "range", "slider":
		return "range"
	case "rating", "stars", "score":
		return "rating"
	default:
		return ""
	}
}

// slugify turns a human label into a safe field key : lowercase alphanumerics,
// runs of other characters collapsed to a single underscore, trimmed.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	pendingSep := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteRune(r)
		} else {
			pendingSep = true
		}
	}
	return b.String()
}

// stringSliceArg coerces a JSON array arg into []string (nil when absent
// or wrong-typed). JSON numbers/bools are stringified defensively.
func stringSliceArg(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		// Weak models sometimes stringify array args ("[\"a\",\"b\"]")
		// instead of emitting a native list. Recover the array.
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		if strings.HasPrefix(s, "[") {
			var arr []any
			if json.Unmarshal([]byte(s), &arr) == nil {
				return stringSliceArg(arr)
			}
		}
		return []string{s}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			} else if e != nil {
				out = append(out, fmt.Sprintf("%v", e))
			}
		}
		return out
	}
	return nil
}

// boolArg coerces a tool arg to bool, tolerating weak models that emit
// "true"/"True"/"1" as strings instead of a native boolean.
func boolArg(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(t))
		return b
	}
	return false
}

// mapSliceArg coerces a JSON array-of-objects arg into []map[string]any
// (nil when absent or wrong-typed) — used for ask_user's form fields.
func mapSliceArg(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if mm, ok := e.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// handleBackgroundRun : dispatch to one of the 5 modes based on
// params, per the doc-defined polymorphic shape.
func (m *MetaDispatcher) handleBackgroundRun(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	// background_run targeting the `agent` delegation tool IS a sub-agent spawn,
	// not a background task. A delegated agent already runs asynchronously and is
	// tracked by the AgentManager (status / wait / list via the `agent` tool,
	// resync via GET /agents), so wrapping it in a background task would hand back
	// a task_id the caller can't correlate to the run. Transform it : spawn the
	// agent ASYNC and return its run_id — "background_run → agent". Handled before
	// the BackgroundManager check because it routes to Agents, not Background. The
	// caller then collects with agent(wait=true, run_id=...) ; handleAgent applies
	// the coordinator-role gate, so this stays as restricted as a direct call.
	if name, _ := call.Args["name"].(string); name != "" && IsAgentSpawnTool(ResolveAlias(Canonicalize(name))) {
		params, perr := coerceParams(call.Args["params"])
		if perr != nil {
			return errored(fmt.Sprintf("background_run: `params` for %q is not valid JSON — %s. Resend `params` as a JSON object.", name, perr.Error()))
		}
		spawn := call
		spawn.Name = ResolveAlias(Canonicalize(name))
		spawn.Args = withoutWait(params) // force async : hand back a run_id, never block
		return m.handleAgent(ctx, spawn)
	}
	if m.Background == nil {
		return errored("background_run not wired (no BackgroundManager)")
	}
	if listTasks, _ := call.Args["list_tasks"].(bool); listTasks {
		statuses, err := m.Background.List(ctx, m.SessionID(call))
		if err != nil {
			return errored("background_run list: " + err.Error())
		}
		return jsonOutcome(map[string]any{"tasks": statuses})
	}
	taskID, _ := call.Args["task_id"].(string)

	// ── signal ─────────────────────────────────────────────────────────────────
	// Send a signal to a running background task. SIGTERM → same as cancel.
	// SIGINT is attempted via an optional Signal interface; falls back to cancel.
	if signal, _ := call.Args["signal"].(string); signal != "" {
		if taskID == "" {
			return errored("background_run signal: 'task_id' is required")
		}
		type signaller interface {
			Signal(ctx context.Context, sessionID, taskID, sig string) error
		}
		switch strings.ToUpper(signal) {
		case "SIGTERM", "TERM", "15":
			if si, ok := m.Background.(signaller); ok {
				if err := si.Signal(ctx, m.SessionID(call), taskID, "SIGTERM"); err != nil {
					return errored("background_run signal: " + err.Error())
				}
				return jsonOutcome(map[string]any{"signalled": taskID, "signal": "SIGTERM"})
			}
			// Fallback: SIGKILL via cancel (no Signal interface)
			_ = m.Background.Cancel(ctx, m.SessionID(call), taskID)
			return jsonOutcome(map[string]any{"signalled": taskID, "signal": "SIGKILL",
				"note": "graceful SIGTERM not supported; sent SIGKILL"})
		case "SIGINT", "INT", "2":
			if si, ok := m.Background.(signaller); ok {
				if err := si.Signal(ctx, m.SessionID(call), taskID, "SIGINT"); err != nil {
					return errored("background_run signal: " + err.Error())
				}
				return jsonOutcome(map[string]any{"signalled": taskID, "signal": "SIGINT"})
			}
			// Fallback: SIGKILL via cancel
			_ = m.Background.Cancel(ctx, m.SessionID(call), taskID)
			return jsonOutcome(map[string]any{"signalled": taskID, "signal": "SIGKILL",
				"note": "graceful SIGINT not supported; sent SIGKILL"})
		default:
			return errored(fmt.Sprintf("background_run signal: unsupported signal %q (use SIGTERM or SIGINT)", signal))
		}
	}

	// ── stdin ──────────────────────────────────────────────────────────────────
	// Send text to a running background task's stdin. Requires the backend to
	// implement an optional SendStdin interface; degrades to a clear error otherwise.
	if stdinIn, _ := call.Args["stdin"].(string); stdinIn != "" && taskID != "" {
		type stdinSender interface {
			SendStdin(ctx context.Context, sessionID, taskID, input string) error
		}
		if ss, ok := m.Background.(stdinSender); ok {
			if err := ss.SendStdin(ctx, m.SessionID(call), taskID, stdinIn); err != nil {
				return errored("background_run stdin: " + err.Error())
			}
			return jsonOutcome(map[string]any{"sent": taskID, "bytes": len(stdinIn)})
		}
		return errored("background_run stdin: stdin injection not supported by this backend — " +
			"feed stdin at launch time via the 'input' param instead")
	}

	// ── watch ──────────────────────────────────────────────────────────────────
	// Run a command repeatedly on an interval, optionally stopping when a pattern
	// appears. Implemented as a background bash.run shell loop — no new process model.
	if watchMode, _ := call.Args["watch"].(bool); watchMode {
		watchCmd, _ := call.Args["command"].(string)
		if watchCmd == "" {
			if p := coerceParamMap(call.Args["params"]); p != nil {
				watchCmd, _ = p["command"].(string)
			}
		}
		if watchCmd == "" {
			return errored("background_run watch: 'command' is required")
		}
		interval := 2.0
		if v, ok := call.Args["interval"].(float64); ok && v > 0 {
			interval = v
		}
		untilPat, _ := call.Args["until"].(string)

		var loopCmd string
		if untilPat != "" {
			loopCmd = fmt.Sprintf(
				`while true; do OUT=$(%s 2>&1); printf -- '--- %%s ---\n' "$(date '+%%H:%%M:%%S')"; printf '%%s\n' "$OUT"; `+
					`if printf '%%s' "$OUT" | grep -qF %s; then printf 'WATCH_MATCHED: %%s\n' %s; break; fi; `+
					`sleep %.0f; done`,
				watchCmd, bgShellQuote(untilPat), bgShellQuote(untilPat), interval)
		} else {
			loopCmd = fmt.Sprintf(
				`while true; do printf -- '--- %%s ---\n' "$(date '+%%H:%%M:%%S')"; %s 2>&1; sleep %.0f; done`,
				watchCmd, interval)
		}

		resolved := ResolveAlias(Canonicalize("bash.run"))
		loopArgs := map[string]any{"command": loopCmd}
		target := runtime.ToolInvocation{
			CallID: call.CallID, Name: resolved, Args: loopArgs,
			AppID: call.AppID, AgentID: call.AgentID,
			UserID: call.UserID, SessionID: call.SessionID,
		}
		if blocked := m.gateTarget(ctx, target); blocked != nil {
			return *blocked
		}
		tid, err := m.Background.Launch(ctx, LaunchRequest{
			SessionID: m.SessionID(call), AppID: call.AppID,
			UserID: call.UserID, AgentID: call.AgentID,
			Tool: resolved, Args: loopArgs,
		})
		if err != nil {
			return errored("background_run watch: " + err.Error())
		}
		if untilPat != "" {
			watchTimeout := 300.0
			if v, ok := call.Args["timeout"].(float64); ok && v > 0 {
				watchTimeout = v
			}
			deadline := time.Now().Add(time.Duration(watchTimeout * float64(time.Second)))
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					break
				}
				st, _ := m.Background.Status(ctx, m.SessionID(call), tid)
				combined := st.Log
				if s, ok := st.Result.(string); ok {
					combined += s
				}
				if strings.Contains(combined, "WATCH_MATCHED:") {
					res := statusToMap(st, 100)
					res["matched"] = untilPat
					_ = m.Background.Cancel(ctx, m.SessionID(call), tid)
					return jsonOutcome(res)
				}
				if st.State != "running" {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
		return jsonOutcome(map[string]any{
			"task_id": tid, "state": "running",
			"note": fmt.Sprintf("watching `%s` every %.0fs — check output with task_id or cancel to stop", watchCmd, interval),
		})
	}

	// ── wait_for ───────────────────────────────────────────────────────────────
	// Poll a running task's live log until a pattern appears or timeout expires.
	// Use with task_id to attach to an already-running task.
	if waitFor, _ := call.Args["wait_for"].(string); waitFor != "" && taskID != "" {
		wfTimeout := 60.0
		if v, ok := call.Args["timeout"].(float64); ok && v > 0 {
			wfTimeout = v
		}
		deadline := time.Now().Add(time.Duration(wfTimeout * float64(time.Second)))
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				break
			}
			st, serr := m.Background.Status(ctx, m.SessionID(call), taskID)
			if serr != nil {
				return errored("background_run wait_for: " + serr.Error())
			}
			combined := st.Log
			if s, ok := st.Result.(string); ok {
				combined += s
			}
			if strings.Contains(combined, waitFor) {
				res := statusToMap(st, 100)
				res["matched"] = waitFor
				return jsonOutcome(res)
			}
			if st.State != "running" {
				return jsonOutcome(map[string]any{
					"task_id": taskID, "state": st.State,
					"note": fmt.Sprintf("task ended before pattern %q appeared", waitFor),
				})
			}
			time.Sleep(500 * time.Millisecond)
		}
		return jsonOutcome(map[string]any{
			"task_id": taskID, "timeout": true,
			"note": fmt.Sprintf("pattern %q not seen within %.0fs", waitFor, wfTimeout),
		})
	}

	if cancel, _ := call.Args["cancel"].(bool); cancel {
		if taskID == "" {
			return errored("background_run cancel: 'task_id' is required")
		}
		if err := m.Background.Cancel(ctx, m.SessionID(call), taskID); err != nil {
			return errored("background_run cancel: " + err.Error())
		}
		return jsonOutcome(map[string]any{"cancelled": taskID})
	}
	// tail_lines lets the agent ask for the last N lines of the live output —
	// the common "give me the last 50/100 lines so I see what's happening".
	// Default 100 lines: enough to spot a build failure or startup error, small
	// enough to not crowd the prompt. tail_lines:0 returns the full 64 KB
	// window for a deep dive.
	tailLines := 100
	if v, ok := call.Args["tail_lines"].(float64); ok {
		tailLines = int(v)
	}
	if wait, _ := call.Args["wait"].(bool); wait {
		if taskID == "" {
			return errored("background_run wait: 'task_id' is required")
		}
		timeout, _ := call.Args["timeout"].(float64)
		st, err := m.Background.Wait(ctx, m.SessionID(call), taskID, timeout)
		if err != nil {
			return errored("background_run wait: " + err.Error())
		}
		return jsonOutcome(statusToMap(st, tailLines))
	}
	if taskID != "" {
		st, err := m.Background.Status(ctx, m.SessionID(call), taskID)
		if err != nil {
			return errored("background_run status: " + err.Error())
		}
		return jsonOutcome(statusToMap(st, tailLines))
	}
	// Launch mode : name + params are required.
	name, _ := call.Args["name"].(string)
	if name == "" {
		return errored("background_run launch: 'name' is required")
	}
	params, perr := coerceParams(call.Args["params"])
	if perr != nil {
		return errored(fmt.Sprintf("background_run: `params` for %q is not valid JSON — %s. Resend `params` as a JSON object.", name, perr.Error()))
	}
	// SG-4 chokepoint : gate the launch target with the caller's real
	// identity BEFORE scheduling, so a denied / approve tool can't be
	// smuggled in as a background task (the manager dispatches later
	// with a tenancy-key AppID that can't be gated downstream).
	// ResolveAlias as well as Canonicalize so the gate + the later background
	// dispatch both see the same resolved FQN execute_tool uses (a bare alias
	// like remember/task_update would otherwise be gated under its unresolved
	// name and fail-closed denied).
	resolved := ResolveAlias(Canonicalize(name))
	target := runtime.ToolInvocation{
		CallID:    call.CallID,
		Name:      resolved,
		Args:      params,
		AppID:     call.AppID,
		AgentID:   call.AgentID,
		UserID:    call.UserID,
		SessionID: call.SessionID,
	}
	if blocked := m.gateTarget(ctx, target); blocked != nil {
		return *blocked
	}
	notifyWhen, _ := call.Args["notify_when"].(string)
	tid, err := m.Background.Launch(ctx, LaunchRequest{
		SessionID:  m.SessionID(call),
		AppID:      call.AppID,
		UserID:     call.UserID,
		AgentID:    call.AgentID,
		Tool:       resolved,
		Args:       params,
		NotifyWhen: notifyWhen,
	})
	if err != nil {
		return errored("background_run launch: " + err.Error())
	}

	// Settle window : give a fast-failing task (a bad port / EADDRINUSE, a crash,
	// a missing module, a syntax error) a moment to fail so the agent gets the
	// error SYNCHRONOUSLY — exactly like a foreground call — instead of a vague
	// "running" followed seconds later by a "[BACKGROUND TASK FAILED]". A healthy
	// long-running server is still alive when the window closes, so it launched
	// OK and keeps running in the background (the agent is woken when it
	// eventually finishes/fails). Default ~2s ; settle_seconds:0 disables it for
	// true fire-and-forget (e.g. a long download you don't want to block on).
	settle := 2.0
	if v, ok := call.Args["settle_seconds"].(float64); ok {
		settle = v
	}
	if settle > 0 {
		if st, werr := m.Background.Wait(ctx, m.SessionID(call), tid, settle); werr == nil {
			// Finished inside the window → return its terminal result (state +
			// error + captured output) so a crash is seen immediately.
			res := statusToMap(st, tailLines)
			res["settled"] = true
			return jsonOutcome(res)
		}
	}
	// If wait_for was also supplied at launch time, enter the polling loop now.
	if waitFor, _ := call.Args["wait_for"].(string); waitFor != "" {
		wfTimeout := 60.0
		if v, ok := call.Args["timeout"].(float64); ok && v > 0 {
			wfTimeout = v
		}
		deadline := time.Now().Add(time.Duration(wfTimeout * float64(time.Second)))
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				break
			}
			st, serr := m.Background.Status(ctx, m.SessionID(call), tid)
			if serr != nil {
				break
			}
			combined := st.Log
			if s, ok := st.Result.(string); ok {
				combined += s
			}
			if strings.Contains(combined, waitFor) {
				res := statusToMap(st, tailLines)
				res["matched"] = waitFor
				res["task_id"] = tid
				return jsonOutcome(res)
			}
			if st.State != "running" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		// Pattern not seen: return task_id so caller can poll manually.
		return jsonOutcome(map[string]any{
			"task_id": tid, "state": "running",
			"note": fmt.Sprintf("task started but pattern %q not seen yet — poll with task_id", waitFor),
		})
	}

	note := "started — still running after the start-up window, so it launched OK and now runs in the background. You'll be notified when it finishes or fails; use background_run with this task_id to check its status/output."
	if notifyWhen != "" {
		note = fmt.Sprintf(
			"started — watching for %q in live output. You will be automatically notified with [BACKGROUND TASK READY] the moment the pattern appears. Continue working — no polling needed.",
			notifyWhen)
	}
	return jsonOutcome(map[string]any{
		"task_id": tid, "state": "running",
		"note":    note,
	})
}

// bgShellQuote single-quote-escapes a string for safe embedding in a shell loop.
func bgShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// withoutWait returns a shallow copy of the params with "wait" removed, so a
// background_run → agent spawn is always asynchronous (returns a run_id) even if
// the model passed wait=true. Returns nil for nil input.
func withoutWait(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		if k == "wait" {
			continue
		}
		out[k] = v
	}
	return out
}

// handleUseSkill : invoke a /command skill from dev.skills (or the
// bundle's skills/ dir). Doc-conform shape (21-skills.md) :
//
//	Param  : { "command": "/commit" }  (leading "/" optional — auto-added)
//	Result : { "success": true,
//	           "data": { "command", "description", "content", "note" } }
//
// The note "Follow these instructions to complete the task." is
// fixed (the Python runtime hard-codes it) so the agent treats the
// returned markdown as actionable instructions, not informational.
func (m *MetaDispatcher) handleUseSkill(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	command, _ := call.Args["command"].(string)
	if command == "" {
		return errored("use_skill: 'command' is required")
	}
	if !strings.HasPrefix(command, "/") {
		command = "/" + command
	}
	if m.SkillLoader == nil {
		return errored("use_skill not wired (no SkillLoader)")
	}
	entry, err := m.SkillLoader.Load(ctx, call.AppID, call.UserID, command)
	if err != nil {
		return errored("use_skill: " + err.Error())
	}
	data := map[string]any{
		"command":     entry.Command,
		"description": entry.Description,
		"content":     entry.Content,
		"note":        "Follow these instructions to complete the task.",
	}
	// Bundled skills ship sibling files and scripts. Without an anchor the
	// agent cannot resolve a relative reference in the markdown ("run
	// scripts/validate.py") and has to guess a path. Handing over the skill's
	// own directory is what turns a text blob into a packaged capability.
	if entry.Dir != "" {
		data["dir"] = entry.Dir
		data["note"] = "Follow these instructions to complete the task. " +
			"Files they reference are relative to `dir` — read or execute them from there."
	}
	return jsonOutcome(map[string]any{"success": true, "data": data})
}

// handleCallApp : invoke another deployed app and return its
// final reply as the tool result. Isolation is preserved by the
// AppCaller — the sub-app runs in its own session, never sharing
// state with the caller's session.
func (m *MetaDispatcher) handleCallApp(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	appID, _ := call.Args["app_id"].(string)
	if appID == "" {
		return errored("call_app: 'app_id' is required")
	}
	prompt, _ := call.Args["prompt"].(string)
	if prompt == "" {
		return errored("call_app: 'prompt' is required")
	}
	if m.AppCaller == nil {
		return errored("call_app not wired (no AppCaller)")
	}
	reply, err := m.AppCaller.Call(ctx, call.AppID, appID, prompt, call.UserID)
	if err != nil {
		return errored("call_app: " + err.Error())
	}
	return jsonOutcome(map[string]any{
		"app_id": appID,
		"reply":  reply,
	})
}

// statusToMap converts a BackgroundStatus into a generic map for
// jsonOutcome. We don't marshal directly because jsonOutcome's
// signature takes map[string]any.
//
// tailLines slices the live log to its most recent N lines when N > 0 — the
// shape the agent typically wants (last 50 / 100 lines is plenty to spot a
// build failure or a startup error without burning the prompt budget on the
// whole 64 KB window). N == 0 returns the full window untrimmed.
func statusToMap(s BackgroundStatus, tailLines int) map[string]any {
	out := map[string]any{
		"task_id": s.TaskID,
		"name":    s.Name,
		"state":   s.State,
	}
	if s.Error != "" {
		out["error"] = s.Error
	}
	if s.Result != nil {
		out["result"] = s.Result
	}
	if s.StartedAt > 0 {
		out["started_at_unix"] = s.StartedAt
	}
	// Live tail of a still-running task (empty once it finishes — the full
	// output is then in result). Lets the agent watch progress / spot a
	// startup error. Sliced to tailLines lines when the caller asked for it,
	// so the agent can keep its context window small while still seeing the
	// most recent output.
	if s.Log != "" {
		log := s.Log
		if tailLines > 0 {
			log = sliceLastLines(log, tailLines)
		}
		out["log"] = log
		// Surface line counts so the agent knows whether more output was
		// dropped, without having to count newlines itself.
		if tailLines > 0 {
			out["log_lines"] = countLines(log)
		}
	}
	return out
}

// sliceLastLines returns the last n lines of s as a single string. Defensive:
// returns s untouched on n<=0 or when s has fewer than n lines.
func sliceLastLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	trimmed := strings.TrimRight(s, "\n")
	parts := strings.Split(trimmed, "\n")
	if len(parts) <= n {
		return s
	}
	return strings.Join(parts[len(parts)-n:], "\n")
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// SessionID returns the session key the BackgroundManager uses to scope
// a session's tasks. The real session id (carried on the invocation) is
// authoritative — it routes lifecycle events to the right realtime room
// and isolates tasks per session. Only when it is absent (older callers
// that don't set it) do we fall back to the (app, user) tenancy pair.
func (m *MetaDispatcher) SessionID(call runtime.ToolInvocation) string {
	if call.SessionID != "" {
		return call.SessionID
	}
	if call.AppID == "" {
		return "anon"
	}
	return call.AppID + ":" + call.UserID
}
