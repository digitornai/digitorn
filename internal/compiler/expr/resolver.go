package expr

import (
	"errors"
	"fmt"
	"strings"
)

func passthrough(x Ref) string {
	out := x.Namespace + "." + strings.Join(x.Path, ".")
	for _, f := range x.Filters {
		out += " | " + f
	}
	return "{{" + out + "}}"
}

// ErrUnresolved is returned by a Resolver when the requested key is not set.
// Fallback chains treat it as "try the next alternative".
var ErrUnresolved = errors.New("unresolved")

// Resolver answers a key inside one namespace.
type Resolver interface {
	Resolve(path []string) (string, error)
}

type ResolverFunc func(path []string) (string, error)

func (f ResolverFunc) Resolve(path []string) (string, error) { return f(path) }

// IncludeResolver loads a YAML fragment from disk and returns its raw text.
type IncludeResolver interface {
	ResolveInclude(path string) (string, error)
}

type Engine struct {
	namespaces  map[string]Resolver
	include     IncludeResolver
	maxDepth    int
	passthrough map[string]struct{}
}

func NewEngine() *Engine {
	return &Engine{namespaces: map[string]Resolver{}, maxDepth: 10, passthrough: map[string]struct{}{}}
}

// AddPassthrough registers extra namespaces that are resolved at runtime, not
// compile time — their `{{ns.path}}` occurrences are left untouched. Used for
// flow node ids, whose `{{<node>.output.x}}` templates the flow runner fills in.
func (e *Engine) AddPassthrough(ns ...string) {
	if e.passthrough == nil {
		e.passthrough = map[string]struct{}{}
	}
	for _, n := range ns {
		e.passthrough[n] = struct{}{}
	}
}

func (e *Engine) Register(namespace string, r Resolver) { e.namespaces[namespace] = r }

func (e *Engine) SetIncludeResolver(r IncludeResolver) { e.include = r }

func (e *Engine) SetMaxDepth(n int) {
	if n > 0 {
		e.maxDepth = n
	}
}

func (e *Engine) HasNamespace(ns string) bool { _, ok := e.namespaces[ns]; return ok }

func (e *Engine) KnownNamespaces() []string {
	out := make([]string, 0, len(e.namespaces))
	for k := range e.namespaces {
		out = append(out, k)
	}
	return out
}

// Eval evaluates an Expr and returns its string value. Returns ErrUnresolved
// when nothing in a fallback chain produced a value.
func (e *Engine) Eval(expr Expr) (string, error) {
	switch x := expr.(type) {
	case Literal:
		return x.Value, nil
	case Ref:
		r, ok := e.namespaces[x.Namespace]
		if !ok {
			if isRuntimeNamespace(x.Namespace) {
				return passthrough(x), nil
			}
			if _, pt := e.passthrough[x.Namespace]; pt {
				return passthrough(x), nil
			}
			return "", fmt.Errorf("unknown namespace %q", x.Namespace)
		}
		v, err := r.Resolve(x.Path)
		if err != nil {
			return "", err
		}
		return v, nil
	case Include:
		if e.include == nil {
			return "", fmt.Errorf("include: not supported in this context")
		}
		return e.include.ResolveInclude(x.Path)
	case Fallback:
		var lastErr error
		for _, alt := range x.Alternatives {
			v, err := e.Eval(alt)
			if err == nil {
				return v, nil
			}
			lastErr = err
			if !errors.Is(err, ErrUnresolved) {
				return "", err
			}
		}
		return "", lastErr
	default:
		return "", fmt.Errorf("unsupported expression type %T", expr)
	}
}
