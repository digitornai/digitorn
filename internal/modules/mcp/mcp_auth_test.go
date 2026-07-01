package mcp

import (
	"encoding/json"
	"os"
	"runtime"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/pkg/module"
)

// The google_keyfile style writes the OAuth client keyfile + credentials file in
// the shapes the Google MCP servers read, and points their env vars at them —
// driven entirely by the catalog entry (env-var names) + the resolved credential,
// not hardcoded per server.
func TestApplyServerAuth_GoogleKeyfile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	m := New()
	ce, ok := catalogLookup("gmail")
	if !ok {
		t.Fatal("gmail not in catalog")
	}
	sc := schema.MCPServerConfig{Transport: "stdio", Command: "npx",
		Auth: &schema.MCPAuthConfig{Type: "oauth2", Provider: "google"}}
	ac := module.AuthContext{
		Provider: "google", ClientID: "cid.apps", ClientSecret: "secret",
		Token: "access-tok", RefreshToken: "refresh-tok",
		Scope: "https://www.googleapis.com/auth/gmail.send", TokenType: "Bearer",
		ExpiresAt: 1_700_000_000, // unix seconds
	}
	spec := m.applyServerAuth(connectSpec{Transport: "stdio", Command: "npx"}, "gmail", sc, ce, ac)

	keyPath := spec.Env["GMAIL_OAUTH_PATH"]
	credPath := spec.Env["GMAIL_CREDENTIALS_PATH"]
	if keyPath == "" || credPath == "" {
		t.Fatalf("keyfile/credentials env vars not set: %+v", spec.Env)
	}
	if spec.AuthFP == "" {
		t.Error("AuthFP must be set so a rotated credential triggers reconnect")
	}

	// gcp-oauth.keys.json — Google "installed app" shape with the client creds.
	var keys struct {
		Installed struct {
			ClientID     string   `json:"client_id"`
			ClientSecret string   `json:"client_secret"`
			RedirectURIs []string `json:"redirect_uris"`
			TokenURI     string   `json:"token_uri"`
		} `json:"installed"`
	}
	mustReadJSON(t, keyPath, &keys)
	if keys.Installed.ClientID != "cid.apps" || keys.Installed.ClientSecret != "secret" {
		t.Errorf("keyfile client creds wrong: %+v", keys.Installed)
	}
	if len(keys.Installed.RedirectURIs) == 0 || keys.Installed.TokenURI == "" {
		t.Errorf("keyfile missing installed-app fields: %+v", keys.Installed)
	}

	// credentials file — "authorized_user" shape with the token + ms expiry.
	var creds struct {
		Type         string `json:"type"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		ExpiryDate   int64  `json:"expiry_date"`
	}
	mustReadJSON(t, credPath, &creds)
	if creds.Type != "authorized_user" || creds.AccessToken != "access-tok" || creds.RefreshToken != "refresh-tok" {
		t.Errorf("credentials shape wrong: %+v", creds)
	}
	if creds.ExpiryDate != 1_700_000_000_000 { // seconds → ms
		t.Errorf("expiry_date must be unix ms, got %d", creds.ExpiryDate)
	}

	// Both files must be 0600 (POSIX only — Windows doesn't honour Unix perm bits).
	if runtime.GOOS != "windows" {
		for _, p := range []string{keyPath, credPath} {
			if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s perms too open: %v", p, fi.Mode().Perm())
			}
		}
	}
}

// The env_token style injects the access token under the env var the catalog
// declares (Notion → NOTION_API_KEY) — no per-server special-casing.
func TestApplyServerAuth_EnvToken(t *testing.T) {
	m := New()
	ce, _ := catalogLookup("notion")
	sc := schema.MCPServerConfig{Transport: "stdio", Command: "mcp-notion",
		Auth: &schema.MCPAuthConfig{Type: "oauth2", Provider: "notion"}}
	ac := module.AuthContext{Provider: "notion", Token: "ntn_secret"} // EnvTokenVar comes from catalog
	spec := m.applyServerAuth(connectSpec{Transport: "stdio"}, "notion", sc, ce, ac)
	if spec.Env["NOTION_API_KEY"] != "ntn_secret" {
		t.Fatalf("env_token not injected from catalog var: %+v", spec.Env)
	}
}

// HTTP transports get NO env injection — the Authorization header is added per
// request by the round-tripper.
func TestApplyServerAuth_HTTPUntouched(t *testing.T) {
	m := New()
	spec := m.applyServerAuth(connectSpec{Transport: "streamable_http", URL: "http://x"},
		"live", schema.MCPServerConfig{Transport: "streamable_http"}, catalogEntry{}, module.AuthContext{Token: "t"})
	if len(spec.Env) != 0 {
		t.Errorf("http transport must not get env injection, got %+v", spec.Env)
	}
}

func mustReadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}
