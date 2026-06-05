package server

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTVerifier validates JWT tokens against a JWKS-managed RSA keyset.
// It is intentionally narrow : RS256 only (matching the Python daemon
// which rejects HS256 explicitly), exp/iss/aud enforced.
type JWTVerifier struct {
	jwks        *JWKS
	issuer      string
	audience    string
	userIDClaim string
	leeway      time.Duration
}

// VerifierClaims is the decoded set of fields the daemon cares about.
// Raw holds the entire claim map for downstream consumers.
type VerifierClaims struct {
	UserID      string
	Email       string
	Name        string
	Roles       []string
	Permissions []string
	IssuedAt    time.Time
	ExpiresAt   time.Time
	Raw         map[string]any
}

var (
	ErrJWTNoToken          = errors.New("jwt: no token")
	ErrJWTUnsupportedAlg   = errors.New("jwt: unsupported alg (RS256 required)")
	ErrJWTMissingKid       = errors.New("jwt: token missing kid")
	ErrJWTInvalidSignature = errors.New("jwt: invalid signature")
	ErrJWTExpired          = errors.New("jwt: token expired")
	ErrJWTNotYet           = errors.New("jwt: token not yet valid")
	ErrJWTWrongIssuer      = errors.New("jwt: issuer mismatch")
	ErrJWTWrongAudience    = errors.New("jwt: audience mismatch")
	ErrJWTMissingUserID    = errors.New("jwt: user_id claim missing")
)

// NewJWTVerifier builds a verifier. user_id_claim defaults to "sub".
func NewJWTVerifier(jwks *JWKS, issuer, audience, userIDClaim string, leeway time.Duration) *JWTVerifier {
	if userIDClaim == "" {
		userIDClaim = "sub"
	}
	if leeway <= 0 {
		leeway = 60 * time.Second
	}
	return &JWTVerifier{
		jwks:        jwks,
		issuer:      issuer,
		audience:    audience,
		userIDClaim: userIDClaim,
		leeway:      leeway,
	}
}

// Verify parses + validates a raw JWT and returns the extracted claims.
func (v *JWTVerifier) Verify(ctx context.Context, token string) (*VerifierClaims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrJWTNoToken
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithLeeway(v.leeway),
		jwt.WithIssuer(v.issuer),
	)
	if v.audience != "" {
		parser = jwt.NewParser(
			jwt.WithValidMethods([]string{"RS256"}),
			jwt.WithLeeway(v.leeway),
			jwt.WithIssuer(v.issuer),
			jwt.WithAudience(v.audience),
		)
	}

	parsed, err := parser.ParseWithClaims(token, jwt.MapClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, ErrJWTUnsupportedAlg
		}
		kid, _ := t.Header["kid"].(string)
		var key *rsa.PublicKey
		var keyErr error
		key, keyErr = v.jwks.GetKey(kid)
		if keyErr != nil {
			return nil, keyErr
		}
		return key, nil
	})
	if err != nil {
		return nil, translateJWTError(err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return nil, ErrJWTInvalidSignature
	}

	uid, _ := claims[v.userIDClaim].(string)
	if uid == "" {
		return nil, ErrJWTMissingUserID
	}

	out := &VerifierClaims{
		UserID: uid,
		Raw:    map[string]any(claims),
	}
	if s, ok := claims["email"].(string); ok {
		out.Email = s
	}
	if s, ok := claims["name"].(string); ok {
		out.Name = s
	}
	out.Roles = stringList(claims["roles"])
	out.Permissions = stringList(claims["perms"])
	if out.Permissions == nil {
		out.Permissions = stringList(claims["permissions"])
	}
	if iat, ok := claims["iat"].(float64); ok {
		out.IssuedAt = time.Unix(int64(iat), 0)
	}
	if exp, ok := claims["exp"].(float64); ok {
		out.ExpiresAt = time.Unix(int64(exp), 0)
	}
	return out, nil
}

func translateJWTError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return ErrJWTExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return ErrJWTNotYet
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return ErrJWTInvalidSignature
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return ErrJWTWrongIssuer
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return ErrJWTWrongAudience
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return ErrJWTInvalidSignature
	}
	return fmt.Errorf("jwt: %w", err)
}

func stringList(v any) []string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}
