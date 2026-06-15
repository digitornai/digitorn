package mcpoauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := gdb.AutoMigrate(&models.Credential{}, &models.OAuthState{}, &models.OAuthClient{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sealer, err := NewSealer(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return NewService(gdb, sealer)
}

func TestServerFromTool(t *testing.T) {
	cases := map[string]string{
		"mcp_github__create_issue":         "github",
		"mcp_google_calendar__list_events": "google_calendar",
		"mcp_srv__x":                       "srv",
		"no_double_underscore":             "",
		"":                                 "",
	}
	for in, want := range cases {
		if got := serverFromTool(in); got != want {
			t.Errorf("serverFromTool(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveAuth_NoOpForNonMCPOrNoLookup(t *testing.T) {
	s := newTestService(t)
	// No serverAuth lookup wired.
	ac, ch, err := s.ResolveAuth(context.Background(), "u", "app", "mcp", "mcp_x__y")
	if err != nil || ac != nil || ch != nil {
		t.Fatalf("want all-nil without lookup, got (%v,%v,%v)", ac, ch, err)
	}
	// Non-mcp module.
	s.SetServerAuthLookup(func(string, string) *schema.MCPAuthConfig {
		return &schema.MCPAuthConfig{Type: "oauth2", Provider: "github"}
	})
	ac, ch, err = s.ResolveAuth(context.Background(), "u", "app", "filesystem", "read")
	if err != nil || ac != nil || ch != nil {
		t.Fatalf("non-mcp should be all-nil, got (%v,%v,%v)", ac, ch, err)
	}
}

func TestResolveAuth_NonOAuthServer(t *testing.T) {
	s := newTestService(t)
	s.SetServerAuthLookup(func(string, string) *schema.MCPAuthConfig { return nil })
	ac, ch, err := s.ResolveAuth(context.Background(), "u", "app", "mcp", "mcp_srv__do")
	if err != nil || ac != nil || ch != nil {
		t.Fatalf("non-oauth server should be all-nil, got (%v,%v,%v)", ac, ch, err)
	}
}

func TestResolveAuth_TokenPresentReturnsAuthContext(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.tokens.Set(ctx, "u", "github", &Token{AccessToken: "AT", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	s.SetServerAuthLookup(func(string, string) *schema.MCPAuthConfig {
		return &schema.MCPAuthConfig{Type: "oauth2", Provider: "github", ClientID: "cid", EnvTokenVar: "GH_TOK"}
	})
	ac, ch, err := s.ResolveAuth(ctx, "u", "app", "mcp", "mcp_srv__do")
	if err != nil {
		t.Fatal(err)
	}
	if ch != nil {
		t.Fatalf("unexpected challenge: %+v", ch)
	}
	if ac == nil || ac.Token != "AT" || ac.EnvTokenVar != "GH_TOK" {
		t.Fatalf("bad auth context: %+v", ac)
	}
}

func TestResolveAuth_PersistsRefreshedExpiry(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"SAME","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	if err := s.tokens.Set(ctx, "u", "custom", &Token{AccessToken: "SAME", RefreshToken: "rt", ExpiresAt: 1}); err != nil {
		t.Fatal(err)
	}
	s.SetServerAuthLookup(func(string, string) *schema.MCPAuthConfig {
		return &schema.MCPAuthConfig{Type: "oauth2", Provider: "custom", ClientID: "c", TokenURL: srv.URL}
	})
	ac, ch, err := s.ResolveAuth(ctx, "u", "app", "mcp", "mcp_srv__do")
	if err != nil || ch != nil || ac == nil {
		t.Fatalf("resolve: ac=%v ch=%v err=%v", ac, ch, err)
	}
	stored, _ := s.tokens.Get(ctx, "u", "custom")
	if stored == nil || stored.ExpiresAt <= 1 {
		t.Fatalf("refreshed expiry not persisted: %+v", stored)
	}
}

func TestResolveAuth_NoTokenReturnsChallenge(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	s.SetServerAuthLookup(func(string, string) *schema.MCPAuthConfig {
		return &schema.MCPAuthConfig{Type: "oauth2", Provider: "github", ClientID: "cid", RedirectURI: "https://cb"}
	})
	ac, ch, err := s.ResolveAuth(ctx, "u", "app", "mcp", "mcp_srv__do")
	if err != nil {
		t.Fatal(err)
	}
	if ac != nil {
		t.Fatalf("unexpected auth context: %+v", ac)
	}
	if ch == nil || ch.Provider != "github" || ch.ServerID != "srv" || ch.AuthURL == "" || ch.State == "" {
		t.Fatalf("bad challenge: %+v", ch)
	}
	// A state row must have been persisted (bound to the user) for the callback.
	var count int64
	s.tokens.db.Model(&models.OAuthState{}).Where("state = ? AND user_id = ?", ch.State, "u").Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 persisted state row, got %d", count)
	}
}
