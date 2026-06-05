// Package middleware is the public SDK for authoring Digitorn middlewares.
//
// A middleware wraps every agent turn — Before runs prior to the LLM call
// (and may short-circuit it) and After runs once the response is returned.
//
// Embed Base in your middleware struct to inherit default no-op hooks:
//
//	type MyMiddleware struct {
//	    middleware.Base
//	}
//
//	func New() *MyMiddleware { return &MyMiddleware{Base: middleware.Base{N: "my_mw"}} }
//
//	func (m *MyMiddleware) Before(ctx context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
//	    // ... custom logic ; return ("", true, nil) to short-circuit the LLM
//	    return "", false, nil
//	}
package middleware

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/ports"
)

// Base embeds in your middleware struct. Override Before and/or After.
type Base struct {
	// N is the middleware name (matches its YAML key, e.g., "mask_secrets").
	N string
}

// Name returns the middleware name.
func (b *Base) Name() string { return b.N }

// Before is a no-op default that does not short-circuit.
func (b *Base) Before(ctx context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	return "", false, nil
}

// After is a no-op default that returns the response unchanged.
func (b *Base) After(ctx context.Context, mctx *ports.MiddlewareContext, response string, toolCalls []ports.LLMToolCall) (string, error) {
	return response, nil
}

// Compile-time assertion.
var _ ports.AppMiddleware = (*Base)(nil)
