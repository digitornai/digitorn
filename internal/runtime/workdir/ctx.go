package workdir

import "context"

type ctxKey struct{}

func WithPathPolicy(ctx context.Context, p PathPolicy) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func PathPolicyFromContext(ctx context.Context) (PathPolicy, bool) {
	if ctx == nil {
		return PathPolicy{}, false
	}
	p, ok := ctx.Value(ctxKey{}).(PathPolicy)
	return p, ok
}
