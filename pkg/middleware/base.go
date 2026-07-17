package middleware

import (
	"context"

	"github.com/digitornai/digitorn/internal/ports"
)

type Base struct {
	N string
}

func (b *Base) Name() string { return b.N }

func (b *Base) Before(ctx context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	return "", false, nil
}

func (b *Base) After(ctx context.Context, mctx *ports.MiddlewareContext, response string, toolCalls []ports.LLMToolCall) (string, error) {
	return response, nil
}

var _ ports.AppMiddleware = (*Base)(nil)
