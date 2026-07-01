package module

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// ToolHandler is the signature shared by tool handlers and middleware-wrapped
// handlers — it lets middlewares chain in an onion pattern (each middleware
// wraps the next handler). It matches tool.Handler.
type ToolHandler func(ctx context.Context, params json.RawMessage) (tool.Result, error)

// ToolMiddleware decorates a tool handler. Implementations should call next
// inside Wrap (or short-circuit with their own Result). The compiler-side
// catalog validates middleware references; the daemon instantiates them and
// hands them to UseMiddleware during module configuration.
type ToolMiddleware interface {
	Name() string
	Wrap(next ToolHandler) ToolHandler
}

// MiddlewareFunc adapts a function to the ToolMiddleware interface for cases
// where defining a struct is overkill.
type MiddlewareFunc struct {
	N  string
	Fn func(ToolHandler) ToolHandler
}

func (m MiddlewareFunc) Name() string                      { return m.N }
func (m MiddlewareFunc) Wrap(next ToolHandler) ToolHandler { return m.Fn(next) }

// UseMiddleware appends a middleware to the module's pipeline. Wrap order is
// FIFO: the first middleware added runs outermost.
func (b *Base) UseMiddleware(mw ...ToolMiddleware) {
	if len(mw) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.middleware == nil {
		b.middleware = make([]ToolMiddleware, 0, len(mw))
	}
	b.middleware = append(b.middleware, mw...)
}

// Middleware returns the currently registered middlewares (snapshot).
func (b *Base) Middleware() []ToolMiddleware {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.middleware) == 0 {
		return nil
	}
	out := make([]ToolMiddleware, len(b.middleware))
	copy(out, b.middleware)
	return out
}

// wrapHandler builds the onion: middlewares applied from last to first so
// that the first added is the outermost layer.
func (b *Base) wrapHandler(h tool.Handler) ToolHandler {
	current := ToolHandler(h)
	b.mu.RLock()
	chain := b.middleware
	b.mu.RUnlock()
	for i := len(chain) - 1; i >= 0; i-- {
		current = chain[i].Wrap(current)
	}
	return current
}

// MiddlewareRegistry maps middleware names to factories. The daemon registers
// every built-in middleware here so YAML configs can name them.
type MiddlewareRegistry struct {
	mu        sync.RWMutex
	factories map[string]func(cfg map[string]any) (ToolMiddleware, error)
}

func NewMiddlewareRegistry() *MiddlewareRegistry {
	return &MiddlewareRegistry{factories: map[string]func(map[string]any) (ToolMiddleware, error){}}
}

func (r *MiddlewareRegistry) Register(name string, f func(cfg map[string]any) (ToolMiddleware, error)) {
	r.mu.Lock()
	r.factories[name] = f
	r.mu.Unlock()
}

func (r *MiddlewareRegistry) Build(name string, cfg map[string]any) (ToolMiddleware, bool, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	mw, err := f(cfg)
	return mw, true, err
}

func (r *MiddlewareRegistry) Names() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.factories))
	for n := range r.factories {
		out = append(out, n)
	}
	r.mu.RUnlock()
	return out
}

// DefaultMiddleware is the process-wide registry of named middleware factories.
var DefaultMiddleware = NewMiddlewareRegistry()
