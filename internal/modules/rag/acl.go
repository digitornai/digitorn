package rag

import (
	"context"

	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

type Filter struct {
	Must map[string][]string
}

func (f Filter) Empty() bool { return len(f.Must) == 0 }

type ACL struct {
	Enabled bool   `json:"enabled"`
	Field   string `json:"field"`
	Scope   string `json:"scope"`
}

func (a ACL) field() string {
	if f := a.Field; f != "" {
		return f
	}
	return "owner"
}

func (e *Engine) aclValue(ctx context.Context) string {
	switch e.cfg.ACL.Scope {
	case "app":
		return pkgmodule.AppID(ctx)
	case "none":
		return ""
	default:
		return pkgmodule.UserID(ctx)
	}
}

func (e *Engine) aclFilter(value string) Filter {
	if !e.cfg.ACL.Enabled || value == "" {
		return Filter{}
	}
	return Filter{Must: map[string][]string{e.cfg.ACL.field(): {value}}}
}

func (e *Engine) docMeta(ctx context.Context, author map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range author {
		out[k] = v
	}
	if e.cfg.ACL.Enabled {
		if v := e.aclValue(ctx); v != "" {
			out[e.cfg.ACL.field()] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func metaMatches(meta map[string]any, f Filter) bool {
	for field, allowed := range f.Must {
		if !valueInAllowed(meta[field], allowed) {
			return false
		}
	}
	return true
}

func valueInAllowed(v any, allowed []string) bool {
	in := func(s string) bool {
		for _, a := range allowed {
			if a == s {
				return true
			}
		}
		return false
	}
	switch x := v.(type) {
	case string:
		return in(x)
	case []string:
		for _, s := range x {
			if in(s) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && in(s) {
				return true
			}
		}
	}
	return false
}
