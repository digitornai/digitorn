package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbathepaul/digitorn/internal/config"
)

func TestCallerCanImpersonate(t *testing.T) {
	cases := []struct {
		name string
		c    *VerifierClaims
		want bool
	}{
		{"nil", nil, false},
		{"empty", &VerifierClaims{UserID: "svc"}, false},
		{"perm", &VerifierClaims{Permissions: []string{"read", permImpersonate}}, true},
		{"role", &VerifierClaims{Roles: []string{"user", roleService}}, true},
		{"other only", &VerifierClaims{Permissions: []string{"read"}, Roles: []string{"user"}}, false},
	}
	for _, tc := range cases {
		if got := callerCanImpersonate(tc.c); got != tc.want {
			t.Errorf("%s: callerCanImpersonate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// runActAs drives the middleware and reports the effective user + actor the inner
// handler observes, plus the HTTP status.
func runActAs(t *testing.T, d *Daemon, header, baseUser string, claims *VerifierClaims) (status int, user, actor string) {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = userIDOf(r.Context())
		actor = actorOf(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := withUserID(req.Context(), baseUser)
	if claims != nil {
		ctx = withClaims(ctx, claims)
	}
	req = req.WithContext(ctx)
	if header != "" {
		req.Header.Set(actAsHeader, header)
	}
	rec := httptest.NewRecorder()
	d.actAsMiddleware(inner).ServeHTTP(rec, req)
	return rec.Code, user, actor
}

// TestActAsMiddleware_StrictRequiresGrant : with auth ON, impersonation needs the
// grant — the session then belongs to the end-user, the caller is recorded as actor,
// and an ungranted attempt is rejected (403). This is the security contract.
func TestActAsMiddleware_StrictRequiresGrant(t *testing.T) {
	d := &Daemon{cfg: &config.Config{Auth: config.Auth{Enabled: true}}}

	if code, user, actor := runActAs(t, d, "", "svc", nil); code != 200 || user != "svc" || actor != "" {
		t.Fatalf("no header must pass through: code=%d user=%q actor=%q", code, user, actor)
	}
	grant := &VerifierClaims{UserID: "svc", Permissions: []string{permImpersonate}}
	if code, user, actor := runActAs(t, d, "bob", "svc", grant); code != 200 || user != "bob" || actor != "svc" {
		t.Fatalf("granted impersonation: code=%d user=%q actor=%q (want 200/bob/svc)", code, user, actor)
	}
	if code, _, _ := runActAs(t, d, "bob", "svc", &VerifierClaims{UserID: "svc"}); code != http.StatusForbidden {
		t.Fatalf("ungranted impersonation must be 403, got %d", code)
	}
}

// TestActAsMiddleware_DevHonorsHeader : with auth OFF (local dev), the header is
// honored unconditionally so the flow is testable without a real token.
func TestActAsMiddleware_DevHonorsHeader(t *testing.T) {
	d := &Daemon{cfg: &config.Config{Auth: config.Auth{Enabled: false}}}
	if code, user, actor := runActAs(t, d, "bob", "anonymous", nil); code != 200 || user != "bob" || actor != "anonymous" {
		t.Fatalf("dev impersonation: code=%d user=%q actor=%q (want 200/bob/anonymous)", code, user, actor)
	}
}
