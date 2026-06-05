package workdir

import "context"

type ctxKey struct{}

// WithPathPolicy returns a context carrying the session's PathPolicy. The
// dispatch chokepoint attaches it once per turn; modules and the chokepoint
// read it back via PathPolicyFromContext.
func WithPathPolicy(ctx context.Context, p PathPolicy) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PathPolicyFromContext returns the PathPolicy carried on ctx, if any. The
// bool is false for non-agent callers (setup steps, CLI, admin) that run
// without a policy — they fall back to a module's own static resolution.
func PathPolicyFromContext(ctx context.Context) (PathPolicy, bool) {
	if ctx == nil {
		return PathPolicy{}, false
	}
	p, ok := ctx.Value(ctxKey{}).(PathPolicy)
	return p, ok
}
