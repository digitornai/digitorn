package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// Action set — the 13 user-facing actions documented in
// docs-site/language/31-tool-hooks.md "Actions (15 built-in)".
// compile_yaml + auto_test_deploy are intentionally NOT exposed :
// they're builder-app scoped and not intended for end-user YAMLs.
//
// Action effects fall in 4 categories :
//
//   1. Pure observability (no veto, no mutation) :
//      log, notify
//
//   2. Conversation mutation (next-turn injection) :
//      inject_message, compact_context
//
//   3. Tool-call interception (synchronous, may block / mutate) :
//      gate           → veto the in-flight tool call
//      transform_params → mutate args before dispatch
//      transform_result → mutate outcome after dispatch
//
//   4. Side-effects (fire-and-forget) :
//      module_action, module_action_inject, shell, pipe, chain,
//      lsp_diagnose
//
// The Engine decides whether to run an action synchronously
// (category 1+3) or asynchronously (category 2+4) based on the
// action type and the firing event. See engine.go.

// GateDecision is the result of running gate actions on tool_start.
// When Allow is false the engine blocks the tool dispatch.
type GateDecision struct {
	Allow  bool
	Reason string
}

// MessageInjection is the result of inject_message — the engine
// projects it into session state at the next turn boundary.
type MessageInjection struct {
	Role        string
	Content     string
	Placeholder string
}

// ActionEffects bundles everything an action may want to write
// back to the engine. Most actions touch zero or one field.
//
// Injects is a slice, not a single pointer : a chain action can emit
// several inject_message effects, and the engine aggregates injects
// across every hook that fires on the same event. Last-wins would
// silently drop all but one — see applyEffects / mergeEffects.
type ActionEffects struct {
	Gate     *GateDecision
	Injects  []*MessageInjection
	Modified bool // true when transform_params/transform_result changed something
}

// RunAction dispatches one action and returns its effects.
// Returns (effects, err) ; err is only set when the action's
// own machinery failed (invalid params, bus error). A `gate`
// action with allow=false is NOT an error — its effects carry
// the veto decision.
func RunAction(
	ctx context.Context, action schema.HookAction, payload Payload,
	deps ActionDeps,
) (ActionEffects, error) {
	switch action.Type {
	case "", schema.ActionNoop:
		return ActionEffects{}, nil
	case "log":
		return ActionEffects{}, runLog(action.Params, payload, deps)
	case "notify":
		return runNotify(ctx, action.Params, payload, deps)
	case "inject_message":
		return runInjectMessage(action.Params, payload)
	case "gate":
		return runGate(action.Params, payload), nil
	case "module_action":
		_, err := runModuleAction(ctx, action.Params, payload, deps)
		return ActionEffects{}, err
	case "module_action_inject":
		return runModuleActionInject(ctx, action.Params, payload, deps)
	case "chain":
		return runChain(ctx, action.Params, payload, deps)
	case "compact_context":
		return ActionEffects{}, runCompactContext(ctx, action.Params, payload, deps)
	case "transform_params":
		return runTransformParams(action.Params, payload)
	case "transform_result":
		return runTransformResult(action.Params, payload)
	case "shell":
		return ActionEffects{}, runShell(ctx, action.Params, payload, deps)
	case "pipe":
		return ActionEffects{}, runPipe(ctx, action.Params, payload, deps)
	case "lsp_diagnose":
		return runLSPDiagnose(ctx, action.Params, payload, deps)
	case "compile_yaml", "auto_test_deploy":
		// Builder-app scoped actions (doc "Actions (15 built-in)" — the
		// last two). Accepted so a builder app compiles ; the builder
		// wires its own executor. In the general runtime they're a
		// deliberate no-op rather than an error.
		return ActionEffects{}, nil
	}
	return ActionEffects{}, fmt.Errorf("hook: unsupported action type %q", action.Type)
}

// ActionDeps is what the runtime injects so actions can reach
// modules, the session bus, the LLM client, etc. Each field is
// optional ; nil = "feature unavailable" (action errors cleanly).
type ActionDeps struct {
	Logger    ActionLogger
	Sink      Sink
	Caller    ToolCaller
	Compactor SessionCompactor
}

// SessionCompactor triggers compaction for one session. Wired by the
// daemon to the sessionstore Compactor ; nil in dev/test, in which
// case compact_context degrades to a clean no-op.
type SessionCompactor interface {
	CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error
}

// ActionLogger is the slice of *slog.Logger the action needs.
type ActionLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// =====================================================================
// log
// =====================================================================

func runLog(params map[string]any, p Payload, deps ActionDeps) error {
	msg := renderTemplate(stringParam(params, "message"), p)
	if msg == "" {
		return fmt.Errorf("log: 'message' required")
	}
	level := strings.ToLower(stringParam(params, "level"))
	if deps.Logger == nil {
		return nil
	}
	switch level {
	case "warn", "warning":
		deps.Logger.Warn(msg, "hook_event", string(p.Event))
	case "error":
		deps.Logger.Error(msg, "hook_event", string(p.Event))
	default:
		deps.Logger.Info(msg, "hook_event", string(p.Event))
	}
	return nil
}

// =====================================================================
// notify  (fires a UI-facing session event so the client renders a toast)
// =====================================================================

func runNotify(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) (ActionEffects, error) {
	if deps.Sink == nil {
		return ActionEffects{}, fmt.Errorf("notify: sink not wired")
	}
	title := renderTemplate(stringParam(params, "title"), p)
	message := renderTemplate(stringParam(params, "message"), p)
	level := stringParam(params, "level")
	if level == "" {
		level = "info"
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventType("hook_notify"),
		SessionID:     p.SessionID,
		AppID:         p.AppID,
		UserID:        p.UserID,
		CorrelationID: p.TurnID,
		Error: &sessionstore.ErrorPayload{
			Source:  "hook.notify",
			Code:    level,
			Message: title + " | " + message,
		},
	}
	if _, err := deps.Sink.AppendDurable(ctx, ev); err != nil {
		return ActionEffects{}, fmt.Errorf("notify: %w", err)
	}
	return ActionEffects{}, nil
}

// =====================================================================
// inject_message  (queue a message for next turn)
// =====================================================================

func runInjectMessage(params map[string]any, p Payload) (ActionEffects, error) {
	content := renderTemplate(stringParam(params, "content"), p)
	if content == "" {
		return ActionEffects{}, fmt.Errorf("inject_message: 'content' required")
	}
	role := stringParam(params, "role")
	if role == "" {
		role = "user"
	}
	placeholder := stringParam(params, "placeholder")
	return ActionEffects{
		Injects: []*MessageInjection{{
			Role:        role,
			Content:     content,
			Placeholder: placeholder,
		}},
	}, nil
}

// =====================================================================
// gate  (veto power on tool_start)
// =====================================================================

func runGate(params map[string]any, p Payload) ActionEffects {
	reason := renderTemplate(stringParam(params, "reason"), p)
	allow, _ := params["allow"].(bool)
	return ActionEffects{
		Gate: &GateDecision{Allow: allow, Reason: reason},
	}
}

// =====================================================================
// transform_params  (modify args before dispatch)
// =====================================================================

func runTransformParams(params map[string]any, p Payload) (ActionEffects, error) {
	raw, ok := params["transformation"].(map[string]any)
	if !ok {
		return ActionEffects{}, fmt.Errorf("transform_params: 'transformation' object required")
	}
	rendered := applyTemplate(raw, p)
	tmap, _ := rendered.(map[string]any)
	if p.ToolArgs == nil {
		return ActionEffects{Modified: false}, nil
	}
	for k, v := range tmap {
		p.ToolArgs[k] = v
	}
	return ActionEffects{Modified: true}, nil
}

// =====================================================================
// transform_result  (modify outcome after dispatch)
// =====================================================================

func runTransformResult(params map[string]any, p Payload) (ActionEffects, error) {
	raw, ok := params["transformation"].(map[string]any)
	if !ok {
		return ActionEffects{}, fmt.Errorf("transform_result: 'transformation' object required")
	}
	rendered := applyTemplate(raw, p)
	tmap, _ := rendered.(map[string]any)
	if p.ToolResult == nil {
		return ActionEffects{Modified: false}, nil
	}
	for k, v := range tmap {
		p.ToolResult[k] = v
	}
	return ActionEffects{Modified: true}, nil
}

// =====================================================================
// module_action  (fire-and-forget call to any tool)
// =====================================================================

func runModuleAction(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) (string, error) {
	if deps.Caller == nil {
		return "", fmt.Errorf("module_action: caller not wired")
	}
	module, _ := params["module"].(string)
	action, _ := params["action"].(string)
	if action == "" {
		return "", fmt.Errorf("module_action: 'action' required")
	}
	target := action
	if module != "" {
		target = module + "." + action
	}
	args := readActionParams(params)
	rendered, _ := applyTemplate(args, p).(map[string]any)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return deps.Caller.Call(ctx, target, rendered)
}

// =====================================================================
// module_action_inject  (call any tool, inject result as next-turn message)
// =====================================================================

func runModuleActionInject(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) (ActionEffects, error) {
	out, err := runModuleAction(ctx, params, p, deps)
	if err != nil {
		return ActionEffects{}, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ActionEffects{}, nil // nothing to inject
	}
	role, _ := params["role"].(string)
	if role == "" {
		role = "user"
	}
	prefix := stringParam(params, "prefix")
	if prefix != "" {
		out = prefix + "\n" + out
	}
	return ActionEffects{
		Injects: []*MessageInjection{{Role: role, Content: out}},
	}, nil
}

// =====================================================================
// chain  (sequential composition)
// =====================================================================

func runChain(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) (ActionEffects, error) {
	raw, ok := params["actions"].([]any)
	if !ok {
		return ActionEffects{}, fmt.Errorf("chain: 'actions' list required")
	}
	var combined ActionEffects
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return combined, fmt.Errorf("chain: actions[%d] is not an object", i)
		}
		actionType, _ := m["type"].(string)
		actionParams := make(map[string]any, len(m))
		for k, v := range m {
			if k == "type" {
				continue
			}
			actionParams[k] = v
		}
		ef, err := RunAction(ctx, schema.HookAction{
			Type:   schema.HookActionType(actionType),
			Params: actionParams,
		}, p, deps)
		if err != nil {
			return combined, fmt.Errorf("chain[%d]: %w", i, err)
		}
		combined = mergeEffects(combined, ef)
	}
	return combined, nil
}

// mergeEffects folds the right hand effects into the left. Later
// effects override earlier ones for the non-additive Gate field ;
// Injects ACCUMULATE (a chain may inject several messages) ; Modified
// is OR'd.
func mergeEffects(a, b ActionEffects) ActionEffects {
	if b.Gate != nil {
		a.Gate = b.Gate
	}
	a.Injects = append(a.Injects, b.Injects...)
	if b.Modified {
		a.Modified = true
	}
	return a
}

// =====================================================================
// shell  (run a command via the shell module)
// =====================================================================

// runShell dispatches the templated command to the shell module's
// `exec` action through the same Caller (and thus security gates) the
// LLM uses. `on_error` ∈ {log (default), ignore, raise} controls how a
// failed command propagates. Doc params : command, cwd, timeout,
// on_error.
func runShell(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) error {
	if deps.Caller == nil {
		return fmt.Errorf("shell: caller not wired")
	}
	command := renderTemplate(stringParam(params, "command"), p)
	if command == "" {
		return fmt.Errorf("shell: 'command' required")
	}
	args := map[string]any{"command": command}
	if cwd := renderTemplate(stringParam(params, "cwd"), p); cwd != "" {
		args["cwd"] = cwd
	}
	if t, ok := params["timeout"]; ok {
		// bash.run names the field timeout_seconds ; the hook doc names it
		// `timeout`. Bridge here.
		args["timeout_seconds"] = t
	}
	// The shell hook action runs through the single, hardened shell module
	// (bash.run) — same `command` contract, plus PATH-enrichment, bash-idiom
	// translation and the safety guard.
	_, cerr := deps.Caller.Call(ctx, "bash.run", args)
	return applyOnError(params, cerr, deps, p, "shell")
}

// =====================================================================
// pipe  (route this tool's output into another tool)
// =====================================================================

// runPipe is the generic tool-chaining primitive. `to` is the target
// tool FQN ; `map` builds its args from templated values (typically
// {{tool.result.*}}) ; `extra` adds static args. Routes through the
// same Caller so the piped call goes through the security pipeline.
func runPipe(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) error {
	if deps.Caller == nil {
		return fmt.Errorf("pipe: caller not wired")
	}
	to := stringParam(params, "to")
	if to == "" {
		return fmt.Errorf("pipe: 'to' is required")
	}
	args := map[string]any{}
	if m, ok := params["map"].(map[string]any); ok {
		if rendered, ok := applyTemplate(m, p).(map[string]any); ok {
			for k, v := range rendered {
				args[k] = v
			}
		}
	}
	if extra, ok := params["extra"].(map[string]any); ok {
		for k, v := range extra {
			args[k] = v
		}
	}
	_, cerr := deps.Caller.Call(ctx, to, args)
	return applyOnError(params, cerr, deps, p, "pipe")
}

// =====================================================================
// lsp_diagnose  (post-write LSP trigger)
// =====================================================================

// runLSPDiagnose resolves the written file's path (and optionally
// content) from the tool params and calls lsp.notify_change via the
// Caller. The action is fully wired ; it requires the `lsp` module to
// be registered to do real work — a clean error surfaces otherwise.
func runLSPDiagnose(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) (ActionEffects, error) {
	if deps.Caller == nil {
		return ActionEffects{}, fmt.Errorf("lsp_diagnose: caller not wired")
	}
	path := resolveEditedPath(params, p)
	if path == "" {
		return ActionEffects{}, fmt.Errorf("lsp_diagnose: could not resolve the edited file path")
	}
	args := map[string]any{"path": path}
	if cf := stringParam(params, "content_field"); cf != "" {
		if cv, ok := resolvePath(cf, p); ok {
			args["content"] = cv
		}
	}
	// Sync the document to the language server and get its diagnostics back.
	// LSP failure (server not installed, in cooldown, unreachable) is non-fatal:
	// log it and return clean so the edit result reaches the agent unchanged.
	// The warning is intentional — it surfaces misconfiguration without breaking
	// the agent's understanding of whether its edit succeeded.
	raw, err := deps.Caller.Call(ctx, "lsp.notify_change", args)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("lsp_diagnose: lsp unavailable, skipping diagnostics",
				"path", path, "err", err.Error())
		}
		return ActionEffects{}, nil
	}
	// Surface the errors/warnings to the agent WITHOUT a separate tool call or chat
	// message — stay silent on a clean file (no noise).
	msg := formatLSPDiagnostics(path, raw)
	if msg == "" {
		return ActionEffects{}, nil
	}
	// Fold the diagnostics straight into the edit/write tool's OWN result (the
	// `text` surface a transform_result mutates), so the agent reads its errors
	// inline as part of the edit it just made — exactly where it expects feedback.
	// lsp_diagnose already runs synchronously on tool_end (see isSyncAction), so
	// the mutation lands before the result reaches the LLM. Fall back to a message
	// only if the result map is unavailable, so the diagnostics are never lost.
	if p.ToolResult != nil {
		prev, _ := p.ToolResult["text"].(string)
		if strings.TrimSpace(prev) != "" {
			p.ToolResult["text"] = prev + "\n\n" + msg
		} else {
			p.ToolResult["text"] = msg
		}
		return ActionEffects{Modified: true}, nil
	}
	return ActionEffects{Injects: []*MessageInjection{{Role: "user", Content: msg}}}, nil
}

// resolveEditedPath finds the edited file's path from the post-tool payload,
// trying the configured path_field then the aliases a model may key the file
// under (file_path / filename / file) and finally the tool's own result path.
func resolveEditedPath(params map[string]any, p Payload) string {
	fields := []string{stringParam(params, "path_field")}
	fields = append(fields,
		"tool.params.path", "tool.params.file_path",
		"tool.params.filename", "tool.params.file", "tool.result.path")
	for _, f := range fields {
		if f == "" {
			continue
		}
		if v, ok := resolvePath(f, p); ok {
			if s, _ := v.(string); strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// formatLSPDiagnostics turns the lsp tool's result into a concise, readable
// error list to inject — or "" when the file is clean AND the project is
// clean, so a passing edit adds no noise. When the edit ripples into other
// files, those are listed under a "[lsp] project" section so the agent sees
// the WHOLE project state after every edit, not just the focal file.
func formatLSPDiagnostics(path, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var r struct {
		Errors      int `json:"errors"`
		Warnings    int `json:"warnings"`
		Diagnostics []struct {
			Line     int    `json:"line"`
			Column   int    `json:"column"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
		Project *struct {
			TotalErrors   int `json:"total_errors"`
			TotalWarnings int `json:"total_warnings"`
			AffectedFiles []struct {
				File     string `json:"file"`
				Errors   int    `json:"errors"`
				Warnings int    `json:"warnings"`
			} `json:"affected_files"`
		} `json:"project"`
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		if strings.Contains(raw, `"ok":true`) || strings.Contains(raw, `"count":0`) {
			return ""
		}
		return "[lsp] diagnostics for " + path + " after your edit:\n" + raw
	}
	projectHasIssues := r.Project != nil && (r.Project.TotalErrors > 0 || r.Project.TotalWarnings > 0)
	if r.Errors == 0 && r.Warnings == 0 && !projectHasIssues {
		return "" // clean here AND across the project — say nothing
	}
	var b strings.Builder
	if r.Errors > 0 || r.Warnings > 0 {
		fmt.Fprintf(&b, "[lsp] %s — %d error(s), %d warning(s) after your edit:",
			filepath.Base(path), r.Errors, r.Warnings)
		for _, d := range r.Diagnostics {
			sev := d.Severity
			if sev == "" {
				sev = "error"
			}
			fmt.Fprintf(&b, "\n  L%d:%d %s: %s", d.Line, d.Column, sev, strings.TrimSpace(d.Message))
		}
	}
	if projectHasIssues {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[lsp] project — %d error(s), %d warning(s) in %d other file(s):",
			r.Project.TotalErrors, r.Project.TotalWarnings, len(r.Project.AffectedFiles))
		for _, f := range r.Project.AffectedFiles {
			fmt.Fprintf(&b, "\n  %s (%d error, %d warning)", f.File, f.Errors, f.Warnings)
		}
	}
	return b.String()
}

// =====================================================================
// compact_context  (trigger session compaction)
// =====================================================================

// runCompactContext triggers compaction of the active session with the
// documented strategy + keep_last. Real when deps.Compactor is wired
// (the daemon binds it to the sessionstore Compactor) ; a clean no-op
// in dev/test so the hook never errors when compaction is unavailable.
func runCompactContext(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) error {
	if deps.Compactor == nil {
		return nil
	}
	strategy := stringParam(params, "strategy")
	keepLast := readActionInt(params, "keep_last")
	return deps.Compactor.CompactSession(ctx, p.SessionID, strategy, keepLast)
}

// =====================================================================
// shared helpers
// =====================================================================

// applyOnError maps a downstream error through the hook's `on_error`
// policy : raise → propagate, ignore → swallow, log (default) → log +
// swallow. Used by shell + pipe.
func applyOnError(params map[string]any, err error, deps ActionDeps, p Payload, action string) error {
	if err == nil {
		return nil
	}
	switch strings.ToLower(stringParam(params, "on_error")) {
	case "raise":
		return err
	case "ignore":
		return nil
	default: // "log" or unset
		if deps.Logger != nil {
			deps.Logger.Warn("hook: "+action+" action failed",
				"err", err.Error(), "hook_event", string(p.Event))
		}
		return nil
	}
}

func readActionInt(params map[string]any, key string) int {
	switch v := params[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// =====================================================================
// helpers
// =====================================================================

func stringParam(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

func readActionParams(params map[string]any) map[string]any {
	// Doc allows `params` OR `action_params` for the call args
	// (module_action / module_action_inject).
	if v, ok := params["params"].(map[string]any); ok {
		return v
	}
	if v, ok := params["action_params"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}
