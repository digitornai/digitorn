// Package toolmw is the per-app tool-call middleware onion. Each layer wraps
// the next around a module tool call (the Go equivalent of the reference
// daemon's MCPCallContext.__call__(ctx, next_)). The pipeline runs daemon-side
// at the dispatch chokepoint, so it wraps in-process modules and the gRPC
// worker round-trip identically.
//
// Two state-scoping rules, enforced per middleware :
//   - app-global (shared across sessions) for resource/health state :
//     circuit_breaker, budget. A dead module is dead for everyone ; a rate
//     limit guards a shared resource.
//   - per-session for result/turn state : dedup, semantic_cache,
//     cross_context. One session's tool output must never surface in another.
package toolmw

import (
	"context"
	"log/slog"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// CallContext is the per-call identity + params handed to every layer. It is
// copied by value through the onion ; a layer that bumps Attempt (retry) only
// changes its own and inner layers' view.
type CallContext struct {
	AppID     string
	SessionID string
	UserID    string
	AgentID   string
	ModuleID  string
	ToolName  string
	Params    []byte
	Attempt   int
}

// Next runs the rest of the onion (and ultimately the module). A layer may
// skip it (cache hit, dedup, open circuit), call it once (the common case),
// or call it repeatedly (retry).
type Next func(ctx context.Context, cc CallContext) (tool.Result, error)

// Middleware is one onion layer.
type Middleware interface {
	Name() string
	Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error)
}

// Pipeline composes layers into one onion. Declaration order = outermost
// first : entry [audit, circuit_breaker, retry] makes audit the outer layer
// (it times the whole thing) and retry the inner one (closest to the module).
type Pipeline struct {
	chain  []Middleware
	logger *slog.Logger
}

// New returns a pipeline over chain, or nil when chain is empty so the caller
// can skip the onion entirely at zero cost.
func New(chain []Middleware, logger *slog.Logger) *Pipeline {
	if len(chain) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{chain: chain, logger: logger}
}

// Names returns the layer names outermost-first (diagnostics / tests).
func (p *Pipeline) Names() []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(p.chain))
	for i, mw := range p.chain {
		out[i] = mw.Name()
	}
	return out
}

// Run wraps terminal in the onion. params is the marshalled tool input ;
// identity comes from the request context (set upstream by the dispatcher).
// This signature structurally satisfies dispatch.ToolPipeline, so no import
// coupling is needed between the two packages.
func (p *Pipeline) Run(ctx context.Context, params []byte, terminal func(context.Context) (tool.Result, error)) (tool.Result, error) {
	if p == nil || len(p.chain) == 0 {
		return terminal(ctx)
	}
	id, _ := tool.IdentityFromContext(ctx)
	cc := CallContext{
		AppID: id.AppID, SessionID: id.SessionID, UserID: id.UserID, AgentID: id.AgentID,
		ModuleID: id.ModuleID, ToolName: id.ToolName, Params: params, Attempt: 1,
	}

	next := Next(func(ctx context.Context, _ CallContext) (tool.Result, error) {
		return terminal(ctx)
	})
	for i := len(p.chain) - 1; i >= 0; i-- {
		mw, inner := p.chain[i], next
		next = func(ctx context.Context, cc CallContext) (tool.Result, error) {
			return mw.Handle(ctx, cc, inner)
		}
	}
	return next(ctx, cc)
}
