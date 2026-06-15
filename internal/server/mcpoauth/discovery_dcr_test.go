package mcpoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func TestParseWWWAuthenticate(t *testing.T) {
	cases := map[string]string{
		`Bearer resource_metadata="https://x/.well-known/oauth-protected-resource"`: "https://x/.well-known/oauth-protected-resource",
		`Bearer realm="x", resource_metadata="https://y/rm", error="bad"`:           "https://y/rm",
		`Bearer error="invalid_token"`:                                              "",
		``:                                                                          "",
	}
	for in, want := range cases {
		if got := parseWWWAuthenticate(in); got != want {
			t.Errorf("parseWWWAuthenticate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWellKnownURLs(t *testing.T) {
	got := wellKnownURLs("https://h.com/mcp", "oauth-authorization-server")
	want := []string{
		"https://h.com/.well-known/oauth-authorization-server/mcp", // RFC 8414 path-aware first
		"https://h.com/.well-known/oauth-authorization-server",     // origin fallback
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("path case: got %v, want %v", got, want)
	}
	got = wellKnownURLs("https://h.com", "openid-configuration")
	if len(got) != 1 || got[0] != "https://h.com/.well-known/openid-configuration" {
		t.Errorf("origin case: got %v", got)
	}
}

func TestProviderKey(t *testing.T) {
	if k := providerKey("github", "https://issuer"); k != "github" {
		t.Errorf("configured provider must win: %q", k)
	}
	if k := providerKey("", "https://mcp.notion.com"); k != "mcp:mcp.notion.com" {
		t.Errorf("issuer-derived key wrong: %q", k)
	}
	if k := providerKey("", "::bad"); k != "custom" {
		t.Errorf("unparsable issuer must fall back to custom: %q", k)
	}
}

// TestListingAuthContext proves an OAuth server's tools can be LISTED using ANY
// already-authorized user's token (specs are user-independent), so the agent
// actually sees them; non-oauth or not-yet-authorized servers yield nil.
func TestListingAuthContext(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.tokens.Set(ctx, "userA", "github", &Token{AccessToken: "TOK-A", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	cfg := &schema.MCPAuthConfig{Type: "oauth2", Provider: "github", ClientID: "cid", RedirectURI: "https://cb"}
	if ac := s.ListingAuthContext(ctx, cfg, "app", "srv"); ac == nil || ac.Token != "TOK-A" {
		t.Fatalf("listing must reuse any user's token, got %+v", ac)
	}
	if s.ListingAuthContext(ctx, &schema.MCPAuthConfig{Type: "api_key"}, "app", "srv") != nil {
		t.Error("non-oauth server must yield nil listing auth")
	}
	if s.ListingAuthContext(ctx, &schema.MCPAuthConfig{Type: "oauth2", Provider: "slack", ClientID: "c", RedirectURI: "r"}, "app", "srv") != nil {
		t.Error("a provider nobody authorized yet must yield nil listing auth")
	}
}

// mockProvider plays the whole server side of a discover→DCR→exchange flow: the
// protected MCP resource (401 + RFC 9728 pointer), its protected-resource
// metadata, the RFC 8414 authorization-server metadata, the RFC 7591 registration
// endpoint, and the token endpoint.
type mockProvider struct {
	srv         *httptest.Server
	registered  int
	exchanged   int
	gotVerifier bool
	gotClientID string
	gotSecret   bool
}

func newMockProvider() *mockProvider {
	m := &mockProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              m.srv.URL,
			"authorization_servers": []string{m.srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           m.srv.URL,
			"authorization_endpoint":           m.srv.URL + "/authorize",
			"token_endpoint":                   m.srv.URL + "/token",
			"registration_endpoint":            m.srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		m.registered++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "dcr-client-123"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		m.exchanged++
		_ = r.ParseForm()
		m.gotVerifier = r.Form.Get("code_verifier") != ""
		m.gotClientID = r.Form.Get("client_id")
		_, m.gotSecret = r.Form["client_secret"]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ACCESS-XYZ", "token_type": "Bearer",
			"expires_in": 3600, "refresh_token": "REFRESH-XYZ",
		})
	})
	// Anything else (the MCP resource itself) → 401 with the RFC 9728 pointer.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+m.srv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	m.srv = httptest.NewServer(mux)
	return m
}

// TestDiscoveryDCRFlow_E2E proves the zero-config generic flow: a server declared
// with ONLY `auth: {type: oauth2}` (no client_id, no URLs, no provider) is taken
// all the way through 401 → RFC 9728 → RFC 8414 → RFC 7591 dynamic registration →
// PKCE authorize URL → code exchange → per-user token → live resolution, against
// a mock authorization server.
func TestDiscoveryDCRFlow_E2E(t *testing.T) {
	mock := newMockProvider()
	defer mock.srv.Close()

	s := newTestService(t)
	s.SetRedirectBase("http://localhost:8000")
	s.SetServerAuthLookup(func(string, string) *schema.MCPAuthConfig {
		return &schema.MCPAuthConfig{Type: "oauth2"} // declared by URL only
	})
	s.SetServerURLLookup(func(string, string) string { return mock.srv.URL })

	ctx := context.Background()
	bare := &schema.MCPAuthConfig{Type: "oauth2"}

	// 1. Authorize → discovery + DCR + PKCE authorize URL.
	authURL, state, err := s.Authorize(ctx, bare, "user-1", "app-1", "srv")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if mock.registered != 1 {
		t.Fatalf("expected exactly one dynamic registration, got %d", mock.registered)
	}
	u, _ := url.Parse(authURL)
	if !strings.HasPrefix(authURL, mock.srv.URL+"/authorize") {
		t.Errorf("authorize URL not from the discovered endpoint: %s", authURL)
	}
	q := u.Query()
	if q.Get("client_id") != "dcr-client-123" {
		t.Errorf("authorize URL missing the DCR client_id: %s", authURL)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("authorize URL missing PKCE: %s", authURL)
	}
	if q.Get("redirect_uri") != "http://localhost:8000/api/oauth/mcp/callback" {
		t.Errorf("redirect_uri wrong: %q", q.Get("redirect_uri"))
	}

	// 2. Callback → exchange the code for a token (public client: PKCE, no secret).
	p, err := s.TakeState(ctx, state)
	if err != nil || p == nil {
		t.Fatalf("take state: p=%v err=%v", p, err)
	}
	if err := s.Exchange(ctx, bare, p, "auth-code"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if mock.exchanged != 1 || !mock.gotVerifier || mock.gotClientID != "dcr-client-123" || mock.gotSecret {
		t.Fatalf("token exchange wrong: exchanged=%d verifier=%v clientID=%q sentSecret=%v",
			mock.exchanged, mock.gotVerifier, mock.gotClientID, mock.gotSecret)
	}

	// 3. ResolveAuth → the agent's next call resolves a live token (no challenge).
	ac, ch, err := s.ResolveAuth(ctx, "user-1", "app-1", "mcp", "mcp_srv__anytool")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ch != nil {
		t.Fatalf("expected a resolved token, got a challenge: %+v", ch)
	}
	if ac == nil || ac.Token != "ACCESS-XYZ" {
		t.Fatalf("resolved AuthContext wrong: %+v", ac)
	}

	// 4. DCR is reused across users/calls — never re-registered.
	if _, ch2, _ := s.ResolveAuth(ctx, "user-2", "app-1", "mcp", "mcp_srv__anytool"); ch2 == nil {
		t.Fatal("a second user with no token should get a challenge")
	}
	if mock.registered != 1 {
		t.Errorf("dynamic registration must be reused across users, got %d registrations", mock.registered)
	}
}
