package validate

import (
	"fmt"

	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/compiler/suggest"
)

// The middleware namespace is split in two : app-level Before/After middleware
// run around the LLM call and are declared under runtime.middleware ;
// tool-call (onion) middleware wrap each module tool call and are declared
// under tools.modules.<id>.middleware. Declaring one on the other surface is a
// silent no-op at runtime, so the compiler rejects it.
//
// appLevelMiddleware mirrors internal/core/middleware.builtins ;
// toolCallMiddleware mirrors internal/toolmw.builtins.
var (
	appLevelMiddleware = []string{
		"mask_secrets", "prompt_inject", "content_filter", "rag_inject", "response_filter",
	}
	toolCallMiddleware = []string{
		"retry", "timeout", "circuit_breaker", "audit", "dedup",
		"semantic_cache", "auto_heal", "cross_context", "budget",
	}
)

func inSet(name string, set []string) bool {
	for _, n := range set {
		if n == name {
			return true
		}
	}
	return false
}

func CheckMiddleware(file string, def *schema.AppDefinition, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if def.Tools == nil {
		return
	}
	for modID, block := range def.Tools.Modules {
		if len(block.Middleware) == 0 {
			continue
		}
		whitelist, hasWhitelist := cat.CompatibleMiddleware(modID)
		whitelistSet := map[string]struct{}{}
		for _, n := range whitelist {
			whitelistSet[n] = struct{}{}
		}
		for i, entry := range block.Middleware {
			name := middlewareName(entry)
			if name == "" {
				continue
			}
			path := fmt.Sprintf("tools.modules.%s.middleware.%d", modID, i)
			// An app-level middleware on the per-module surface is in the
			// catalog (so HasMiddleware passes) but the tool-call pipeline can't
			// run it — catch the misplacement with a precise message.
			if inSet(name, appLevelMiddleware) {
				bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
					"%s: %q is an app-level middleware — declare it under runtime.middleware, not per-module",
					path, name))
				continue
			}
			if !cat.HasMiddleware(name) {
				s, _ := suggest.Closest(name, cat.MiddlewareNames(), 2)
				d := diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
					"%s: unknown middleware %q", path, name)
				if s != "" {
					d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
				}
				bag.Add(d)
				continue
			}
			if hasWhitelist {
				if _, ok := whitelistSet[name]; !ok {
					bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
						"%s: middleware %q is not compatible with module %q (allowed: %v)",
						path, name, modID, whitelist))
					continue
				}
			}
			checkMiddlewareConfig(name, middlewareConfig(entry, name), path, bag)
		}
	}
}

// CheckAppMiddleware validates the app-level runtime.middleware list : each
// entry must name a documented built-in (or "custom" for a plugin). An unknown
// name is almost always a typo, so it is an error with a suggestion.
func CheckAppMiddleware(file string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	if def.Runtime == nil {
		return
	}
	for i, e := range def.Runtime.Middleware {
		if e.Name == "" {
			continue
		}
		path := fmt.Sprintf("runtime.middleware.%d", i)
		// A `custom` entry resolves to an out-of-process gRPC plugin, which
		// needs the worker `module` and `kind` to dispatch to. Both are
		// mandatory : without them the runtime can only skip the entry with a
		// warning, so catch the misconfiguration at compile time instead.
		if e.Name == "custom" {
			if s, _ := e.Config["module"].(string); s == "" {
				bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
					"%s: custom middleware requires a non-empty `module`", path))
			}
			if s, _ := e.Config["kind"].(string); s == "" {
				bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
					"%s: custom middleware requires a non-empty `kind` (worker pool)", path))
			}
			continue
		}
		// A tool-call middleware on the app-level surface is the mirror mistake.
		if inSet(e.Name, toolCallMiddleware) {
			bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
				"%s: %q is a tool-call middleware — declare it under tools.modules.<id>.middleware, not runtime.middleware",
				path, e.Name))
			continue
		}
		if inSet(e.Name, appLevelMiddleware) {
			checkMiddlewareConfig(e.Name, e.Config, path, bag)
			continue
		}
		d := diagnostic.Errorf(diagnostic.CodeUnknownMiddleware, posUnknown,
			"%s: unknown middleware %q (built-ins: %v, or \"custom\")",
			path, e.Name, appLevelMiddleware)
		if s, ok := suggest.Closest(e.Name, appLevelMiddleware, 2); ok {
			d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
		}
		bag.Add(d)
	}
}

// ---- config bounds --------------------------------------------------------

// numBound is one numeric config constraint : value must be >= min (or > min
// when excl) and, when hasMax, <= max.
type numBound struct {
	key    string
	min    float64
	excl   bool
	max    float64
	hasMax bool
}

// middlewareConfigBounds catches the numeric misconfigurations that would
// otherwise silently fall back to a default (or disable the middleware) at
// runtime. Bounds mirror each constructor's clamping in
// internal/toolmw and internal/core/middleware.
var middlewareConfigBounds = map[string][]numBound{
	"retry":           {{key: "max_attempts", min: 1}, {key: "base_delay", min: 0}, {key: "max_delay", min: 0}},
	"timeout":         {{key: "seconds", min: 0, excl: true}},
	"circuit_breaker": {{key: "failure_threshold", min: 1}, {key: "recovery_timeout", min: 0}, {key: "half_open_calls", min: 1}},
	"dedup":           {{key: "window_seconds", min: 0}, {key: "max_entries", min: 1}},
	"semantic_cache":  {{key: "similarity_threshold", min: 0, max: 1, hasMax: true}, {key: "ttl", min: 0}, {key: "max_entries", min: 1}},
	"cross_context":   {{key: "max_entries", min: 1}, {key: "summary_max_chars", min: 0}},
	"budget":          {{key: "max_calls_per_hour", min: 0}, {key: "cost_per_call", min: 0}, {key: "max_cost_per_hour", min: 0}},
	"response_filter": {{key: "max_length", min: 0}},
}

func checkMiddlewareConfig(name string, cfg map[string]any, path string, bag *diagnostic.Bag) {
	for _, b := range middlewareConfigBounds[name] {
		v, ok := cfgNum(cfg, b.key)
		if !ok {
			continue
		}
		bad := v < b.min || (b.excl && v == b.min) || (b.hasMax && v > b.max)
		if !bad {
			continue
		}
		want := fmt.Sprintf(">= %g", b.min)
		switch {
		case b.hasMax:
			want = fmt.Sprintf("in [%g, %g]", b.min, b.max)
		case b.excl:
			want = fmt.Sprintf("> %g", b.min)
		}
		bag.Add(diagnostic.Errorf(diagnostic.CodeOutOfRange, posUnknown,
			"%s: %s.%s = %g is out of range (must be %s)", path, name, b.key, v, want))
	}
}

func cfgNum(cfg map[string]any, key string) (float64, bool) {
	if cfg == nil {
		return 0, false
	}
	switch v := cfg[key].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

func middlewareName(entry map[string]any) string {
	if n, ok := entry["name"].(string); ok && n != "" {
		return n
	}
	if len(entry) == 1 {
		for k := range entry {
			return k
		}
	}
	return ""
}

// middlewareConfig extracts the config block of a per-module middleware entry,
// for either YAML form : {name: X, config: {...}} or {X: {...}}.
func middlewareConfig(entry map[string]any, name string) map[string]any {
	if c, ok := entry["config"].(map[string]any); ok {
		return c
	}
	if c, ok := entry[name].(map[string]any); ok {
		return c
	}
	return nil
}
