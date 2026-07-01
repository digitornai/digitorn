package middleware

import (
	"log/slog"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/ports"
)

// constructor builds one middleware instance from its YAML config + injected
// deps. Adding a built-in = one entry in builtins. The `custom` plugin
// transport (gRPC) registers here too, so resolution stays uniform.
type constructor func(cfg map[string]any, deps Deps) (ports.AppMiddleware, error)

// builtins is the name -> constructor registry for the documented app-level
// middleware. It is the single source of truth the compiler validation and the
// runtime build both read.
var builtins = map[string]constructor{
	"mask_secrets":    newMaskSecrets,
	"prompt_inject":   newPromptInject,
	"content_filter":  newContentFilter,
	"rag_inject":      newRagInject,
	"response_filter": newResponseFilter,
}

// BuiltinNames returns the registered built-in middleware names (for
// compile-time validation + suggestions).
func BuiltinNames() []string {
	out := make([]string, 0, len(builtins))
	for n := range builtins {
		out = append(out, n)
	}
	return out
}

// IsBuiltin reports whether name is a registered built-in middleware.
func IsBuiltin(name string) bool {
	_, ok := builtins[name]
	return ok
}

// Build assembles the app-level middleware pipeline from runtime.middleware,
// in declaration order. Disabled entries (enabled: false) are skipped ; unknown
// names are logged and skipped (never fatal). Returns nil when no middleware is
// active, so the engine can skip the pipeline entirely at zero cost.
func Build(entries []schema.MiddlewareEntry, deps Deps, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	var chain []ports.AppMiddleware
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		if e.Enabled != nil && !*e.Enabled {
			continue
		}
		var (
			mw  ports.AppMiddleware
			err error
		)
		switch {
		case e.Name == "custom":
			if deps.CustomFactory == nil {
				logger.Warn("app_middleware_custom_not_wired", slog.String("name", e.Name))
				continue
			}
			mw, err = deps.CustomFactory(e.Name, e.Config)
		default:
			ctor, ok := builtins[e.Name]
			if !ok {
				logger.Warn("app_middleware_unknown", slog.String("name", e.Name))
				continue
			}
			mw, err = ctor(e.Config, deps)
		}
		if err != nil {
			logger.Warn("app_middleware_config_error", slog.String("name", e.Name), slog.String("err", err.Error()))
			continue
		}
		chain = append(chain, mw)
	}
	if len(chain) == 0 {
		return nil
	}
	return New(chain, logger)
}

// ---- config accessors (lenient: YAML/JSON numeric + string handling) ------

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

func cfgBool(cfg map[string]any, key string, def bool) bool {
	if cfg == nil {
		return def
	}
	if b, ok := cfg[key].(bool); ok {
		return b
	}
	return def
}

func cfgStrSlice(cfg map[string]any, key string) []string {
	if cfg == nil {
		return nil
	}
	switch v := cfg[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
