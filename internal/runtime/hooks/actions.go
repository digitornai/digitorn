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


type GateDecision struct {
	Allow  bool
	Reason string
}

type MessageInjection struct {
	Role        string
	Content     string
	Placeholder string
}


type ActionEffects struct {
	Gate     *GateDecision
	Injects  []*MessageInjection
	Modified bool
}


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
	
		return ActionEffects{}, nil
	}
	return ActionEffects{}, fmt.Errorf("hook: unsupported action type %q", action.Type)
}


type ActionDeps struct {
	Logger    ActionLogger
	Sink      Sink
	Caller    ToolCaller
	Compactor SessionCompactor
}


type SessionCompactor interface {
	CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error
}


type ActionLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}


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


func runGate(params map[string]any, p Payload) ActionEffects {
	reason := renderTemplate(stringParam(params, "reason"), p)
	allow, _ := params["allow"].(bool)
	return ActionEffects{
		Gate: &GateDecision{Allow: allow, Reason: reason},
	}
}



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



func runModuleActionInject(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) (ActionEffects, error) {
	out, err := runModuleAction(ctx, params, p, deps)
	if err != nil {
		return ActionEffects{}, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ActionEffects{}, nil
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
		args["timeout_seconds"] = t
	}

	_, cerr := deps.Caller.Call(ctx, "bash.run", args)
	return applyOnError(params, cerr, deps, p, "shell")
}


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

	raw, err := deps.Caller.Call(ctx, "lsp.notify_change", args)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("lsp_diagnose: lsp unavailable, skipping diagnostics",
				"path", path, "err", err.Error())
		}
		return ActionEffects{}, nil
	}

	msg := formatLSPDiagnostics(path, raw)
	if msg == "" {
		return ActionEffects{}, nil
	}

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
		return ""
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


func runCompactContext(ctx context.Context, params map[string]any, p Payload, deps ActionDeps) error {
	if deps.Compactor == nil {
		return nil
	}
	strategy := stringParam(params, "strategy")
	keepLast := readActionInt(params, "keep_last")
	return deps.Compactor.CompactSession(ctx, p.SessionID, strategy, keepLast)
}


func applyOnError(params map[string]any, err error, deps ActionDeps, p Payload, action string) error {
	if err == nil {
		return nil
	}
	switch strings.ToLower(stringParam(params, "on_error")) {
	case "raise":
		return err
	case "ignore":
		return nil
	default:
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


func stringParam(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

func readActionParams(params map[string]any) map[string]any {
	if v, ok := params["params"].(map[string]any); ok {
		return v
	}
	if v, ok := params["action_params"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}
