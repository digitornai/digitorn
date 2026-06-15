package daemonclient

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// mkJWT builds an UNSIGNED token whose payload carries the given claims (header
// and signature are irrelevant — CanImpersonate only decodes the payload).
func mkJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
}

// CanImpersonate must mirror the daemon's callerCanImpersonate exactly : a
// "sessions:impersonate" or "*" permission, or the "service" role.
func TestCanImpersonate(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   bool
	}{
		{"impersonate perm", map[string]any{"perms": []any{"read", "sessions:impersonate"}}, true},
		{"wildcard perm", map[string]any{"perms": []any{"*"}}, true},
		{"permissions alias", map[string]any{"permissions": []any{"sessions:impersonate"}}, true},
		{"service role", map[string]any{"roles": []any{"user", "service"}}, true},
		{"admin role only", map[string]any{"roles": []any{"admin"}}, false}, // admin alone ≠ grant
		{"plain user", map[string]any{"perms": []any{"read"}, "roles": []any{"user"}}, false},
		{"empty", map[string]any{}, false},
	}
	for _, c := range cases {
		if got := CanImpersonate(mkJWT(c.claims)); got != c.want {
			t.Errorf("%s: CanImpersonate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanImpersonate_BadTokens(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "only.one", "h..s"} {
		if CanImpersonate(tok) {
			t.Errorf("malformed token %q must not grant impersonation", tok)
		}
	}
}
