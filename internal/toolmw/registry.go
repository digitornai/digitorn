package toolmw

import (
	"context"
	"log/slog"
)

// Embedder turns text into a vector for semantic_cache. nil in Deps makes
// semantic_cache inert (matches the reference, which no-ops without a model).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ToolSuggestion is one alternative tool auto_heal can propose on failure.
type ToolSuggestion struct {
	ModuleID    string
	ToolName    string
	Description string
}

// ToolResolver returns alternatives for a failed (module, tool) call. nil in
// Deps makes auto_heal inert.
type ToolResolver func(moduleID, toolName string) []ToolSuggestion

// Deps are the runtime dependencies injected into middleware at build time,
// kept out of the YAML config.
type Deps struct {
	Embedder     Embedder
	ToolResolver ToolResolver
	Logger       *slog.Logger
}

type constructor func(cfg map[string]any, deps Deps) (Middleware, error)

// builtins is the name -> constructor registry for the documented tool-call
// middleware. Single source of truth for compiler validation and runtime build.
var builtins = map[string]constructor{
	"retry":           newRetry,
	"timeout":         newTimeout,
	"circuit_breaker": newCircuitBreaker,
	"audit":           newAudit,
	"dedup":           newDedup,
	"semantic_cache":  newSemanticCache,
	"auto_heal":       newAutoHeal,
	"cross_context":   newCrossContext,
	"budget":          newBudget,
}

// BuiltinNames returns the registered tool-call middleware names.
func BuiltinNames() []string {
	out := make([]string, 0, len(builtins))
	for n := range builtins {
		out = append(out, n)
	}
	return out
}

// IsBuiltin reports whether name is a registered tool-call middleware.
func IsBuiltin(name string) bool {
	_, ok := builtins[name]
	return ok
}

// Build assembles a pipeline from a per-module middleware list
// (tools.modules.<id>.middleware), in declaration order. Disabled entries are
// skipped ; unknown names are logged and skipped (never fatal). Returns nil
// when nothing is active so the dispatcher skips the onion at zero cost.
func Build(entries []map[string]any, deps Deps, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	deps.Logger = logger
	var chain []Middleware
	for _, e := range entries {
		name, cfg, enabled := entryFields(e)
		if name == "" || !enabled {
			continue
		}
		ctor, ok := builtins[name]
		if !ok {
			logger.Warn("tool_middleware_unknown", slog.String("name", name))
			continue
		}
		mw, err := ctor(cfg, deps)
		if err != nil {
			logger.Warn("tool_middleware_config_error", slog.String("name", name), slog.String("err", err.Error()))
			continue
		}
		chain = append(chain, mw)
	}
	return New(chain, logger)
}

// entryFields normalises a per-module middleware entry. Two YAML forms are
// accepted, mirroring validate.middlewareName :
//
//	{ name: retry, config: {...}, enabled: false }   # structured
//	{ retry: { max_attempts: 3 } }                   # name-as-key
func entryFields(e map[string]any) (name string, cfg map[string]any, enabled bool) {
	enabled = true
	if n, ok := e["name"].(string); ok && n != "" {
		name = n
		if c, ok := e["config"].(map[string]any); ok {
			cfg = c
		}
		if b, ok := e["enabled"].(bool); ok {
			enabled = b
		}
		return name, cfg, enabled
	}
	if len(e) == 1 {
		for k, v := range e {
			name = k
			if c, ok := v.(map[string]any); ok {
				cfg = c
				if b, ok := c["enabled"].(bool); ok {
					enabled = b
				}
			}
		}
	}
	return name, cfg, enabled
}

// ---- lenient config accessors (YAML/JSON numeric + string handling) -------

func cfgStr(cfg map[string]any, key, def string) string {
	if cfg == nil {
		return def
	}
	if s, ok := cfg[key].(string); ok {
		return s
	}
	return def
}

func cfgInt(cfg map[string]any, key string, def int) int {
	if cfg == nil {
		return def
	}
	switch v := cfg[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

func cfgFloat(cfg map[string]any, key string, def float64) float64 {
	if cfg == nil {
		return def
	}
	switch v := cfg[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return def
}

func cfgBool(cfg map[string]any, key string, def bool) bool {
	if cfg == nil {
		return def
	}
	if b, ok := cfg[key].(bool); ok {
		return b
	}
	return def
}
