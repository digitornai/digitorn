package daemonclient

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// CanImpersonate reports whether a service JWT carries the grant the daemon
// requires to act on behalf of end-users — the SAME contract the daemon enforces
// in callerCanImpersonate: a "sessions:impersonate" or "*" permission, or the
// "service" role. The token is only DECODED (its payload, unverified) to read
// claims; the daemon does the real cryptographic verification. The background
// service calls this at boot so a mis-scoped credential is caught with a clear
// warning instead of a confusing 403 on every user-owned session wake.
//
// In PRODUCTION the background service must run with a dedicated service token
// (role "service" or permission "sessions:impersonate"), never a human admin
// token — that is the only credential that lets a scheduled/channel wake run AS
// the real end-user while the service is recorded as the actor for audit.
func CanImpersonate(jwt string) bool {
	claims := decodeJWTClaims(jwt)
	if claims == nil {
		return false
	}
	for _, p := range claimStrings(claims["perms"], claims["permissions"]) {
		if p == "sessions:impersonate" || p == "*" {
			return true
		}
	}
	for _, r := range claimStrings(claims["roles"]) {
		if r == "service" {
			return true
		}
	}
	return false
}

// decodeJWTClaims base64url-decodes a JWT's payload segment into a claims map.
// No signature verification — callers use it only to inspect locally-held tokens.
func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// claimStrings flattens the given claim values (each a []any of strings, a
// []string, or a single string) into one []string, dropping empties.
func claimStrings(vals ...any) []string {
	var out []string
	for _, v := range vals {
		switch t := v.(type) {
		case string:
			if t != "" {
				out = append(out, t)
			}
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok && s != "" {
					out = append(out, s)
				}
			}
		case []string:
			for _, s := range t {
				if s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}
