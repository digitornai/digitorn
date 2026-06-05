package llm

import "context"

type userJWTCtxKey struct{}

// WithUserJWT carries the gateway bearer on the context so mid-turn callers that
// don't thread the TurnInput — notably context compaction's summary-brain call —
// can authenticate to the gateway exactly like the main turn (which sets
// ChatRequest.UserJWT directly). Empty jwt is a no-op.
func WithUserJWT(ctx context.Context, jwt string) context.Context {
	if jwt == "" {
		return ctx
	}
	return context.WithValue(ctx, userJWTCtxKey{}, jwt)
}

// UserJWTFromContext returns the gateway bearer carried by WithUserJWT, or "".
func UserJWTFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(userJWTCtxKey{}).(string)
	return s
}
