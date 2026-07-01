// Package middleware implements the Before/After pipeline executor for agent turns.
package middleware

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/digitornai/digitorn/internal/ports"
)

// Pipeline is an ordered chain of middlewares applied around an LLM call.
type Pipeline struct {
	chain  []ports.AppMiddleware
	logger *slog.Logger
}

// New creates a pipeline from the given chain (executed in slice order).
func New(chain []ports.AppMiddleware, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{chain: chain, logger: logger}
}

// Before runs every Before hook in declaration order. The first middleware to
// signal shortCircuit stops the chain ; its returned string becomes the
// response and the caller must NOT invoke the LLM.
func (p *Pipeline) Before(ctx context.Context, mctx *ports.MiddlewareContext) (response string, shortCircuit bool, err error) {
	for _, mw := range p.chain {
		out, sc, err := mw.Before(ctx, mctx)
		if err != nil {
			return "", false, fmt.Errorf("middleware %s.Before: %w", mw.Name(), err)
		}
		if sc {
			p.logger.Info("middleware short-circuit", slog.String("middleware", mw.Name()))
			return out, true, nil
		}
	}
	return "", false, nil
}

// After runs every After hook in REVERSE declaration order (mirrors the
// reference daemon), threading the response through them. It runs even after a
// Before short-circuit, with the short-circuit text and empty tool calls.
func (p *Pipeline) After(ctx context.Context, mctx *ports.MiddlewareContext, response string, toolCalls []ports.LLMToolCall) (string, error) {
	current := response
	for i := len(p.chain) - 1; i >= 0; i-- {
		mw := p.chain[i]
		out, err := mw.After(ctx, mctx, current, toolCalls)
		if err != nil {
			return current, fmt.Errorf("middleware %s.After: %w", mw.Name(), err)
		}
		current = out
	}
	return current, nil
}

// Names returns the names of all middlewares in the chain.
func (p *Pipeline) Names() []string {
	names := make([]string, len(p.chain))
	for i, mw := range p.chain {
		names[i] = mw.Name()
	}
	return names
}
