// Package lsp gives an agent live code diagnostics. It speaks the Language
// Server Protocol (JSON-RPC over a server's stdio) to any installed language
// server — gopls, pyright, typescript-language-server, rust-analyzer, texlab,
// … — so a new language is one config line, no code. After the agent edits a
// file the lsp_diagnose hook calls notify_change, which syncs the document to
// the server and returns the errors/warnings it reports. The backend behind a
// file is pluggable (see manager.backend): LSP today, compiler/linter later.
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

// Config is the per-app configuration. It is applied via Init because the lsp
// module runs in a worker (worker/runner.go calls Init with the app config).
type Config struct {
	Servers         map[string]ServerConfig `json:"servers" yaml:"servers"`
	SettleSeconds   float64                 `json:"settle_seconds" yaml:"settle_seconds"`
	DisableBuiltins bool                    `json:"disable_builtins" yaml:"disable_builtins"`
}

// ServerConfig declares one language server: how to launch it and which files
// it owns.
type ServerConfig struct {
	Command     string   `json:"command" yaml:"command"`           // e.g. "gopls" or "pyright-langserver --stdio"
	Extensions  []string `json:"extensions" yaml:"extensions"`     // e.g. [".go"]
	RootMarkers []string `json:"root_markers" yaml:"root_markers"` // e.g. ["go.mod"]
	Protocol    string   `json:"protocol" yaml:"protocol"`         // "lsp" (default)
}

// maxOpTimeout is a hard ceiling on ANY lsp operation, independent of the
// caller's context. The lsp_diagnose hook is fire-and-forget and best-effort:
// if a language server hangs (slow cold start, wedged process), the operation
// must still return so the async hook goroutine can never leak and a failure is
// simply skipped — diagnostics never block or slow the agent loop.
const maxOpTimeout = 30 * time.Second

// Module is the lsp module instance.
type Module struct {
	module.Base

	mu  sync.RWMutex
	mgr *manager
}

// New constructs the lsp module with its tools wired.
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

// Stop shuts down every running language server.
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

// flexContent accepts the file body in whatever shape the model emits:
//
//   - a plain JSON string        → used as-is
//   - an array of strings        → joined with "\n" (models often send lines this way)
//   - an array of objects        → the first of "text"/"content"/"line"/"value" key
//                                  found on each object is extracted, then joined
//   - any other scalar (number…) → converted via fmt.Sprintf
//
// Prevents "cannot unmarshal array into … of type string" when an LLM
// structures file content as a line array instead of a single string.
type flexContent string

// Object keys we recognize as carrying string content. Order matters: first match wins.
var (
	flexArrayObjectKeys  = []string{"text", "content", "line", "value", "code", "source", "snippet"}
	flexObjectStringKeys = []string{"content", "text", "body", "code", "source"}
)

func (f *flexContent) UnmarshalJSON(b []byte) error {
	// 1. Explicit null
	if string(b) == "null" {
		*f = ""
		return nil
	}

	// 2. Fast path : plain JSON string
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexContent(s)
		return nil
	}

	// 3. Array path : []any — each element becomes a line. A bad element fails
	// the WHOLE unmarshal: silently embedding raw JSON as source code would
	// produce nonsense diagnostics the LLM would take at face value.
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

	// 4. Object path
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err == nil {
		if sv, ok := firstStringField(obj, flexObjectStringKeys); ok {
			*f = flexContent(sv)
			return nil
		}
		return fmt.Errorf("lsp: content is an object with no string field in %v (got keys %v)", flexObjectStringKeys, sortedKeys(obj))
	}

	// 5. Scalar (number, bool…) — refuse instead of stringifying it as code.
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

// buildSpecs turns the config into server specs: app-declared servers first
// (so they win on overlapping extensions), then the built-in defaults unless
// disabled.
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
	// Embed the project rollup only when it carries signal — keeps the wire
	// payload small for the common "everything is fine" case.
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
