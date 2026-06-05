package server

import (
	"context"
	"errors"
	"strings"
)

// JWTAuthValidator implements AuthValidator using a JWTVerifier. It is
// pluggable into the SocketIOBridge in place of NullAuth. Dev mode (when
// enabled by config) falls back to the `user_id` metadata field so local
// development without a real auth service still works.
type JWTAuthValidator struct {
	Verifier *JWTVerifier
	DevMode  bool
}

func (v *JWTAuthValidator) Validate(ctx context.Context, token string, metadata map[string]any) (*AuthIdentity, error) {
	token = strings.TrimSpace(token)

	// If a JWT is present, verify it — even in dev mode (prefer the strict path).
	if token != "" && v.Verifier != nil {
		claims, err := v.Verifier.Verify(ctx, token)
		if err == nil {
			return &AuthIdentity{
				UserID:       claims.UserID,
				Capabilities: claims.Permissions,
				Roles:        claims.Roles,
			}, nil
		}
		if !v.DevMode {
			return nil, err
		}
	}
	if v.DevMode {
		uid, _ := metadata["user_id"].(string)
		if uid == "" {
			uid = "anonymous"
		}
		return &AuthIdentity{UserID: uid}, nil
	}
	return nil, errors.New("auth: no token")
}
