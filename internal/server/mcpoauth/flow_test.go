package mcpoauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func boolPtr(b bool) *bool { return &b }

func TestResolveAuth_ProviderMerge(t *testing.T) {
	t.Run("google fills urls, keeps pkce on", func(t *testing.T) {
		ra := resolveAuth(&schema.MCPAuthConfig{Provider: "google", ClientID: "cid"})
		if ra.AuthorizeURL == "" || ra.TokenURL == "" {
			t.Fatal("urls not filled from table")
		}
		if !ra.PKCE {
			t.Fatal("google should keep pkce on")
		}
		if ra.TokenAuthMethod != "body" {
			t.Fatalf("want body, got %q", ra.TokenAuthMethod)
		}
	})

	t.Run("github table turns pkce off", func(t *testing.T) {
		ra := resolveAuth(&schema.MCPAuthConfig{Provider: "github"})
		if ra.PKCE {
			t.Fatal("github table should turn pkce off")
		}
	})

	t.Run("notion is basic with owner=user", func(t *testing.T) {
		ra := resolveAuth(&schema.MCPAuthConfig{Provider: "notion"})
		if ra.TokenAuthMethod != "basic" {
			t.Fatalf("want basic, got %q", ra.TokenAuthMethod)
		}
		if ra.ExtraAuthorize["owner"] != "user" {
			t.Fatalf("missing owner=user: %v", ra.ExtraAuthorize)
		}
		if ra.PKCE {
			t.Fatal("notion pkce should be off")
		}
	})

	t.Run("custom keeps yaml urls and default pkce on", func(t *testing.T) {
		ra := resolveAuth(&schema.MCPAuthConfig{
			AuthorizeURL: "https://x/auth", TokenURL: "https://x/token",
		})
		if ra.Provider != "custom" {
			t.Fatalf("want custom, got %q", ra.Provider)
		}
		if ra.AuthorizeURL != "https://x/auth" || !ra.PKCE {
			t.Fatal("custom should keep yaml urls + default pkce on")
		}
	})

	t.Run("explicit yaml pkce false survives table", func(t *testing.T) {
		ra := resolveAuth(&schema.MCPAuthConfig{Provider: "google", PKCE: boolPtr(false)})
		if ra.PKCE {
			t.Fatal("explicit pkce=false must survive (override fires only when current true)")
		}
	})

	t.Run("explicit yaml auth method survives table", func(t *testing.T) {
		ra := resolveAuth(&schema.MCPAuthConfig{Provider: "notion", TokenAuthMethod: "body"})
		// notion table is basic, but only overrides when current is "body" → so it DOES flip.
		if ra.TokenAuthMethod != "basic" {
			t.Fatalf("notion override should apply over body default, got %q", ra.TokenAuthMethod)
		}
	})
}

func TestBuildAuthorizeURL(t *testing.T) {
	ra := resolveAuth(&schema.MCPAuthConfig{
		Provider: "notion", ClientID: "cid", RedirectURI: "https://cb",
		Scopes: []string{"a", "b"},
	})
	raw := buildAuthorizeURL(ra, "st8", "chal")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("redirect_uri") != "https://cb" {
		t.Fatal("client_id/redirect_uri missing")
	}
	if q.Get("response_type") != "code" || q.Get("state") != "st8" {
		t.Fatal("response_type/state missing")
	}
	if q.Get("scope") != "a b" {
		t.Fatalf("scope not space-joined: %q", q.Get("scope"))
	}
	if q.Get("owner") != "user" {
		t.Fatal("notion extra param missing")
	}
	// notion has pkce off → no challenge
	if q.Get("code_challenge") != "" {
		t.Fatal("pkce-off provider should not carry a challenge")
	}

	ra2 := resolveAuth(&schema.MCPAuthConfig{Provider: "google", ClientID: "c", RedirectURI: "r"})
	q2 := mustQuery(t, buildAuthorizeURL(ra2, "s", "chal"))
	if q2.Get("code_challenge") != "chal" || q2.Get("code_challenge_method") != "S256" {
		t.Fatal("pkce provider should carry S256 challenge")
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestExchange_BodyProvider(t *testing.T) {
	var gotCT, gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotForm = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	f := NewFlow()
	ra := resolvedAuth{TokenURL: srv.URL, TokenAuthMethod: "body", ClientID: "cid", ClientSecret: "sec"}
	tok, err := f.exchange(context.Background(), ra, "thecode", "https://cb", "verif")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		t.Fatalf("body provider must POST form, got CT %q", gotCT)
	}
	if !strings.Contains(gotForm, "client_secret=sec") || !strings.Contains(gotForm, "code_verifier=verif") {
		t.Fatalf("form missing creds/verifier: %q", gotForm)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" || tok.ExpiresAt == 0 {
		t.Fatalf("bad token parse: %+v", tok)
	}
}

func TestExchange_BasicProvider_UsesJSONAndBasicAuth(t *testing.T) {
	var gotCT string
	var gotUser, gotPass string
	var okBasic bool
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotUser, gotPass, okBasic = r.BasicAuth()
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT"}`))
	}))
	defer srv.Close()

	f := NewFlow()
	ra := resolvedAuth{TokenURL: srv.URL, TokenAuthMethod: "basic", ClientID: "cid", ClientSecret: "sec"}
	if _, err := f.exchange(context.Background(), ra, "c", "https://cb", ""); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("basic provider must POST json, got %q", gotCT)
	}
	if !okBasic || gotUser != "cid" || gotPass != "sec" {
		t.Fatalf("basic auth header wrong: %q/%q ok=%v", gotUser, gotPass, okBasic)
	}
	if body["client_secret"] != nil {
		t.Fatal("basic provider must NOT put client_secret in the body")
	}
}

// TestRefresh_BasicUsesSameEncoding is the bug #4 fix: refresh must use the same
// content-type as exchange (JSON+Basic for basic providers), not a form body.
func TestRefresh_BasicUsesSameEncoding(t *testing.T) {
	var gotCT string
	var okBasic bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_, _, okBasic = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT2"}`))
	}))
	defer srv.Close()

	f := NewFlow()
	ra := resolvedAuth{TokenURL: srv.URL, TokenAuthMethod: "basic", ClientID: "cid", ClientSecret: "sec"}
	tok, err := f.refresh(context.Background(), ra, "RT")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !strings.HasPrefix(gotCT, "application/json") || !okBasic {
		t.Fatalf("refresh on basic provider must use json+basic, got CT=%q basic=%v", gotCT, okBasic)
	}
	if tok.AccessToken != "AT2" {
		t.Fatalf("bad refreshed token: %+v", tok)
	}
}

func TestExchange_ProviderErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code"}`))
	}))
	defer srv.Close()
	f := NewFlow()
	ra := resolvedAuth{TokenURL: srv.URL, TokenAuthMethod: "body", ClientID: "c", ClientSecret: "s"}
	if _, err := f.exchange(context.Background(), ra, "c", "cb", ""); err == nil {
		t.Fatal("expected error from provider error response")
	}
}
