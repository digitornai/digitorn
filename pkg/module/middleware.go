package module

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

type ToolHandler func(ctx context.Context, params json.RawMessage) (tool.Result, error)

type ToolMiddleware interface {
	Name() string
	Wrap(next ToolHandler) ToolHandler
}

type MiddlewareFunc struct {
	N  string
	Fn func(ToolHandler) ToolHandler
}

func (m MiddlewareFunc) Name() string                      { return m.N }
func (m MiddlewareFunc) Wrap(next ToolHandler) ToolHandler { return m.Fn(next) }

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

var DefaultMiddleware = NewMiddlewareRegistry()
