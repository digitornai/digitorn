package module

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// SecurityGate decides whether a tool call is allowed for the current caller.
// The runtime supplies a concrete gate (driven by the app's capabilities
// block); the module asks the gate at every Invoke.
type SecurityGate interface {
	Authorize(ctx context.Context, moduleID, toolName string, params json.RawMessage) error
}

// PolicyEnforcer applies rate limits, cooldowns, and concurrency caps. Its
// Acquire is called before the handler runs; the returned release closure
// runs after (success or error).
type PolicyEnforcer interface {
	Acquire(ctx context.Context, moduleID, toolName string) (release func(), err error)
}

// ErrUnauthorized is returned when SecurityGate refuses a call.
var ErrUnauthorized = errors.New("unauthorized")

// ErrRateLimited is returned when PolicyEnforcer blocks a call.
var ErrRateLimited = errors.New("rate limited")

// gateKey / policyKey live on context.Context; setting them lets a caller
// (typically the runtime) inject enforcement without modules knowing how.
type gateKey struct{}
type policyKey struct{}

func WithSecurityGate(ctx context.Context, g SecurityGate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, gateKey{}, g)
}

func GateFrom(ctx context.Context) SecurityGate {
	g, _ := ctx.Value(gateKey{}).(SecurityGate)
	return g
}

func WithPolicyEnforcer(ctx context.Context, p PolicyEnforcer) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, policyKey{}, p)
}

func PolicyFrom(ctx context.Context) PolicyEnforcer {
	p, _ := ctx.Value(policyKey{}).(PolicyEnforcer)
	return p
}

// applyGuards is the centralised entry where Base.Invoke calls the gate and
// the policy enforcer. Both are optional — if the ctx carries neither, the
// call goes straight through.
func applyGuards(ctx context.Context, moduleID, toolName string, params json.RawMessage) (release func(), err error) {
	if g := GateFrom(ctx); g != nil {
		if err := g.Authorize(ctx, moduleID, toolName, params); err != nil {
			return nil, err
		}
	}
	if p := PolicyFrom(ctx); p != nil {
		return p.Acquire(ctx, moduleID, toolName)
	}
	return func() {}, nil
}

// resultFor packages an error as a typed tool.Result so callers don't have to
// branch on err vs Result.Success themselves.
func resultFor(err error) tool.Result {
	if err == nil {
		return tool.Result{Success: true}
	}
	return tool.Result{Success: false, Error: err.Error()}
}
