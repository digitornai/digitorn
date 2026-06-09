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
	"strings"
	"sync"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/pkg/module"
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

	// 3. Array path : []any — each element becomes a line
	var arr []any
	if err := json.Unmarshal(b, &arr); err == nil {
		lines := make([]string, 0, len(arr))
		for _, el := range arr {
			switch v := el.(type) {
			case string:
				lines = append(lines, v)
			case map[string]any:
				found := false
				for _, k := range []string{"text", "content", "line", "value", "code", "source", "snippet"} {
					if sv, ok := v[k].(string); ok {
						lines = append(lines, sv)
						found = true
						break
					}
				}
				if !found {
					b2, _ := json.Marshal(v)
					lines = append(lines, string(b2))
				}
			default:
				b2, _ := json.Marshal(el)
				lines = append(lines, string(b2))
			}
		}
		*f = flexContent(strings.Join(lines, "\n"))
		return nil
	}

	// 4. Object path
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err == nil {
		for _, k := range []string{"content", "text", "body", "code", "source"} {
			if sv, ok := obj[k].(string); ok {
				*f = flexContent(sv)
				return nil
			}
		}
		// Fallback: serialize the object so the LLM sees its mistake
		b2, _ := json.MarshalIndent(obj, "", "  ")
		*f = flexContent(string(b2))
		return nil
	}

	// 5. Scalar fallback (number, bool…)
	*f = flexContent(strings.Trim(string(b), `"`))
	return nil
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
	return diagnosticsResult(p.Path, diags), nil
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
	return diagnosticsResult(p.Path, diags), nil
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

func diagnosticsResult(path string, diags []Diagnostic) tool.Result {
	errs, warns := 0, 0
	for _, d := range diags {
		switch d.Severity {
		case "error":
			errs++
		case "warning":
			warns++
		}
	}
	return tool.Result{
		Success: true,
		Data: map[string]any{
			"path":        path,
			"diagnostics": diags,
			"count":       len(diags),
			"errors":      errs,
			"warnings":    warns,
			"ok":          errs == 0,
		},
		Display: &tool.DisplayHint{Type: "json", Title: fmt.Sprintf("LSP: %d error(s), %d warning(s)", errs, warns)},
	}
}

func errResult(err error) tool.Result {
	return tool.Result{Success: false, Error: err.Error()}
}
