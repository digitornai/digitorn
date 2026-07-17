package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

type Config struct {
	Servers         map[string]ServerConfig `json:"servers" yaml:"servers"`
	SettleSeconds   float64                 `json:"settle_seconds" yaml:"settle_seconds"`
	DisableBuiltins bool                    `json:"disable_builtins" yaml:"disable_builtins"`
}

type ServerConfig struct {
	Command     string   `json:"command" yaml:"command"`
	Extensions  []string `json:"extensions" yaml:"extensions"`
	RootMarkers []string `json:"root_markers" yaml:"root_markers"`
	Protocol    string   `json:"protocol" yaml:"protocol"`
}

const maxOpTimeout = 30 * time.Second

type Module struct {
	module.Base

	mu  sync.RWMutex
	mgr *manager
}

func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          "lsp",
		Version:     "1.0.0",
		Description: "Live code diagnostics (errors/warnings) via Language Server Protocol.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}

	m.RegisterTool(module.Tool{
		Name: "notify_change",
		Description: "Report that a file changed and return the language server's current diagnostics " +
			"(errors/warnings) for it. Normally triggered automatically by the lsp_diagnose hook after an edit.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "Path of the changed file.", Required: true, Path: true},
			{Name: "content", Type: "string", Description: "New file content. Omit to let the server read it from disk."},
		},
		RiskLevel: tool.RiskLow,
		Tags:      []string{"lsp", "diagnostics", "code"},
		Handler:   m.notifyChange,
	})

	m.RegisterTool(module.Tool{
		Name:        "diagnostics",
		Description: "Return the current diagnostics (errors/warnings) the language server reports for a file.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "File path to inspect.", Required: true, Path: true},
		},
		RiskLevel: tool.RiskLow,
		Tags:      []string{"lsp", "diagnostics", "code"},
		Handler:   m.diagnostics,
	})

	return m
}

func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	var c Config
	if err := m.BindConfig(cfg, &c); err != nil {
		return err
	}
	settle := 10 * time.Second
	if c.SettleSeconds > 0 {
		settle = time.Duration(c.SettleSeconds * float64(time.Second))
	}
	mgr := newManager(buildSpecs(c), settle)
	m.mu.Lock()
	m.mgr = mgr
	m.mu.Unlock()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	mgr := m.mgr
	m.mu.Unlock()
	if mgr != nil {
		mgr.stopAll(ctx)
	}
	return m.Base.Stop(ctx)
}

func (m *Module) manager() *manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mgr
}

type flexContent string

var (
	flexArrayObjectKeys  = []string{"text", "content", "line", "value", "code", "source", "snippet"}
	flexObjectStringKeys = []string{"content", "text", "body", "code", "source"}
)

func (f *flexContent) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexContent(s)
		return nil
	}

	var arr []any
	if err := json.Unmarshal(b, &arr); err == nil {
		lines := make([]string, 0, len(arr))
		for i, el := range arr {
			switch v := el.(type) {
			case string:
				lines = append(lines, v)
			case map[string]any:
				sv, ok := firstStringField(v, flexArrayObjectKeys)
				if !ok {
					return fmt.Errorf("lsp: content[%d] is an object with no string field in %v (got keys %v) — send the file body as a string or pick one of those keys", i, flexArrayObjectKeys, sortedKeys(v))
				}
				lines = append(lines, sv)
			case float64, bool, nil:
				return fmt.Errorf("lsp: content[%d] is a %T; expected string or {%s:string}", i, el, strings.Join(flexArrayObjectKeys, "|"))
			default:
				return fmt.Errorf("lsp: content[%d] is %T; expected string or object", i, el)
			}
		}
		*f = flexContent(strings.Join(lines, "\n"))
		return nil
	}

	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err == nil {
		if sv, ok := firstStringField(obj, flexObjectStringKeys); ok {
			*f = flexContent(sv)
			return nil
		}
		return fmt.Errorf("lsp: content is an object with no string field in %v (got keys %v)", flexObjectStringKeys, sortedKeys(obj))
	}

	return fmt.Errorf("lsp: content has unsupported shape %q; pass the file body as a string", strings.TrimSpace(string(b)))
}

func firstStringField(m map[string]any, keys []string) (string, bool) {
	for _, k := range keys {
		if sv, ok := m[k].(string); ok {
			return sv, true
		}
	}
	return "", false
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type changeParams struct {
	Path    string       `json:"path"`
	Content *flexContent `json:"content"`
}

func (m *Module) notifyChange(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p changeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	if strings.TrimSpace(p.Path) == "" {
		err := fmt.Errorf("path is required")
		return errResult(err), err
	}
	mgr := m.manager()
	if mgr == nil {
		err := fmt.Errorf("lsp module not initialized")
		return errResult(err), err
	}

	ctx, cancel := context.WithTimeout(ctx, maxOpTimeout)
	defer cancel()

	content := ""
	if p.Content != nil {
		content = string(*p.Content)
	} else {
		c, err := readFileText(p.Path)
		if err != nil {
			return errResult(fmt.Errorf("read %s: %w", p.Path, err)), err
		}
		content = c
	}

	diags, err := mgr.notifyChange(ctx, p.Path, content)
	if err != nil {
		return errResult(err), err
	}
	return diagnosticsResult(p.Path, diags, mgr.projectSummary(ctx, p.Path)), nil
}

type diagParams struct {
	Path string `json:"path"`
}

func (m *Module) diagnostics(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p diagParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	if strings.TrimSpace(p.Path) == "" {
		err := fmt.Errorf("path is required")
		return errResult(err), err
	}
	mgr := m.manager()
	if mgr == nil {
		err := fmt.Errorf("lsp module not initialized")
		return errResult(err), err
	}
	ctx, cancel := context.WithTimeout(ctx, maxOpTimeout)
	defer cancel()
	diags, err := mgr.diagnostics(ctx, p.Path)
	if err != nil {
		return errResult(err), err
	}
	return diagnosticsResult(p.Path, diags, mgr.projectSummary(ctx, p.Path)), nil
}

func buildSpecs(c Config) []serverSpec {
	var specs []serverSpec
	for name, sc := range c.Servers {
		argv := strings.Fields(sc.Command)
		if len(argv) == 0 {
			continue
		}
		proto := sc.Protocol
		if proto == "" {
			proto = "lsp"
		}
		specs = append(specs, serverSpec{
			name: name, protocol: proto, argv: argv,
			extensions: sc.Extensions, rootMarkers: sc.RootMarkers,
		})
	}
	if !c.DisableBuiltins {
		specs = append(specs, builtinSpecs()...)
	}
	return specs
}

func diagnosticsResult(path string, diags []Diagnostic, project ProjectSummary) tool.Result {
	errs, warns := 0, 0
	for _, d := range diags {
		switch d.Severity {
		case "error":
			errs++
		case "warning":
			warns++
		}
	}
	data := map[string]any{
		"path":        path,
		"diagnostics": diags,
		"count":       len(diags),
		"errors":      errs,
		"warnings":    warns,
		"ok":          errs == 0 && project.TotalErrors == 0,
	}
	if project.TotalErrors > 0 || project.TotalWarnings > 0 {
		data["project"] = project
	}
	title := fmt.Sprintf("LSP: %d error(s), %d warning(s)", errs, warns)
	if project.TotalErrors > 0 || project.TotalWarnings > 0 {
		title += fmt.Sprintf(" | project: +%d / +%d across %d file(s)",
			project.TotalErrors, project.TotalWarnings, len(project.AffectedFiles))
	}
	return tool.Result{
		Success: true,
		Data:    data,
		Display: &tool.DisplayHint{Type: "json", Title: title},
	}
}

func errResult(err error) tool.Result {
	return tool.Result{Success: false, Error: err.Error()}
}
