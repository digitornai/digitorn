package mcpoauth

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func TestBuildAuthorizeURL_GoogleRequestsDurableOffline(t *testing.T) {
	ra := resolvedAuth{
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		ClientID:     "cid",
		RedirectURI:  "https://auth.digitorn.ai/oauth/mcp/callback",
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly"},
	}
	u := buildAuthorizeURL(ra, "state123", "")
	for _, want := range []string{"access_type=offline", "prompt=consent"} {
		if !strings.Contains(u, want) {
			t.Errorf("google authorize URL missing %q: %s", want, u)
		}
	}
}

func TestBuildAuthorizeURL_MicrosoftGetsOfflineAccessScope(t *testing.T) {
	ra := resolvedAuth{
		AuthorizeURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		ClientID:     "cid",
		Scopes:       []string{"User.Read"},
	}
	u := buildAuthorizeURL(ra, "s", "")
	if !strings.Contains(u, "offline_access") {
		t.Errorf("microsoft authorize URL must request offline_access scope: %s", u)
	}
}

func TestBuildAuthorizeURL_MergesPreExistingQuery(t *testing.T) {
	ra := resolvedAuth{
		AuthorizeURL: "https://slack.com/oauth/v2/authorize?user_scope=search:read,chat:write",
		ClientID:     "cid",
		RedirectURI:  "https://auth.digitorn.ai/oauth/mcp/callback",
		Scopes:       []string{"channels:read", "chat:write"},
	}
	u := buildAuthorizeURL(ra, "st", "")
	if strings.Count(u, "?") != 1 {
		t.Fatalf("authorize URL must have exactly one '?': %s", u)
	}
	if !strings.Contains(u, "user_scope=search") {
		t.Errorf("pre-existing user_scope must be preserved: %s", u)
	}
	if !strings.Contains(u, "client_id=cid") || !strings.Contains(u, "state=st") {
		t.Errorf("standard params must be present: %s", u)
	}
}

func TestResolveAuth_MicrosoftCloudPlaceholderSubstituted(t *testing.T) {
	cfg := &schema.MCPAuthConfig{
		Type:         "oauth2",
		Provider:     "custom",
		AuthorizeURL: "https://{cloud}/common/oauth2/v2.0/authorize",
		TokenURL:     "https://{cloud}/common/oauth2/v2.0/token",
		ClientID:     "cid",
		Scopes:       []string{"Mail.ReadWrite", "offline_access"},
	}
	ra := resolveAuth(cfg)
	if strings.Contains(ra.AuthorizeURL, "{cloud}") || strings.Contains(ra.TokenURL, "{cloud}") {
		t.Fatalf("{cloud} must be substituted: authorize=%s token=%s", ra.AuthorizeURL, ra.TokenURL)
	}
	if !strings.Contains(ra.AuthorizeURL, "login.microsoftonline.com") {
		t.Errorf("authorize URL must resolve to login.microsoftonline.com: %s", ra.AuthorizeURL)
	}
	u := buildAuthorizeURL(ra, "st", "")
	if !strings.Contains(u, "offline_access") {
		t.Errorf("microsoft authorize URL must carry offline_access: %s", u)
	}
}

func TestBuildAuthorizeURL_NotionRequestsOwnerUser(t *testing.T) {
	ra := resolvedAuth{
		AuthorizeURL: "https://api.notion.com/v1/oauth/authorize",
		ClientID:     "cid",
		RedirectURI:  "https://auth.digitorn.ai/oauth/mcp/callback",
	}
	u := buildAuthorizeURL(ra, "st", "")
	if !strings.Contains(u, "owner=user") {
		t.Errorf("notion authorize URL must carry owner=user: %s", u)
	}
}

func TestBuildAuthorizeURL_NonGoogleUnchanged(t *testing.T) {
	ra := resolvedAuth{
		AuthorizeURL: "https://github.com/login/oauth/authorize",
		ClientID:     "cid",
		Scopes:       []string{"repo"},
	}
	u := buildAuthorizeURL(ra, "s", "")
	if strings.Contains(u, "access_type") || strings.Contains(u, "offline_access") {
		t.Errorf("github authorize URL must not gain google/microsoft params: %s", u)
	}
}

func TestExplicitExtraAuthorizeWins(t *testing.T) {
	ra := resolvedAuth{
		AuthorizeURL:   "https://accounts.google.com/o/oauth2/v2/auth",
		ClientID:       "cid",
		ExtraAuthorize: map[string]string{"prompt": "select_account"},
	}
	u := buildAuthorizeURL(ra, "s", "")
	if !strings.Contains(u, "prompt=select_account") {
		t.Errorf("explicit ExtraAuthorize must override the durable default: %s", u)
	}
	if !strings.Contains(u, "access_type=offline") {
		t.Errorf("access_type default should still apply: %s", u)
	}
}
