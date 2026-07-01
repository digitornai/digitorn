package bifrost

import (
	"context"
	"errors"
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/digitornai/digitorn/internal/llm"
)

// H6 — Account-level negative paths. The routing decision in
// GetKeysForProvider must reject invalid combinations FAST and CLEAR,
// so the daemon never makes an external call with the wrong credential
// (or no credential at all).

// TestAccountNegatives_BYOKMissingAPIKey : BYOK=true without APIKey
// must be rejected before any network call.
func TestAccountNegatives_BYOKMissingAPIKey(t *testing.T) {
	acc := newAccount(Config{})
	ctx := context.WithValue(context.Background(), ctxKeyRoute, &routeInfo{
		BYOK:   true,
		APIKey: "", // missing on purpose
	})
	_, err := acc.GetKeysForProvider(ctx, schemas.Anthropic)
	if err == nil {
		t.Fatal("expected error : BYOK without APIKey must fail")
	}
	if !contains(err.Error(), "APIKey") && !contains(err.Error(), "api") {
		t.Errorf("error should mention missing APIKey, got: %v", err)
	}
}

// TestAccountNegatives_GatewayMissingUserJWT : BYOK=false without
// UserJWT must be rejected before any network call.
func TestAccountNegatives_GatewayMissingUserJWT(t *testing.T) {
	acc := newAccount(Config{})
	ctx := context.WithValue(context.Background(), ctxKeyRoute, &routeInfo{
		BYOK:    false,
		UserJWT: "", // missing on purpose
	})
	_, err := acc.GetKeysForProvider(ctx, schemas.OpenAI)
	if err == nil {
		t.Fatal("expected error : gateway routing without UserJWT must fail")
	}
	if !contains(err.Error(), "UserJWT") && !contains(err.Error(), "jwt") {
		t.Errorf("error should mention missing UserJWT, got: %v", err)
	}
}

// TestAccountNegatives_NoRouteInfoInContext : if no route info has
// been stashed (programmer error somewhere upstream), the account
// must fail loudly rather than silently default.
func TestAccountNegatives_NoRouteInfoInContext(t *testing.T) {
	acc := newAccount(Config{})
	_, err := acc.GetKeysForProvider(context.Background(), schemas.Anthropic)
	if err == nil {
		t.Fatal("expected error when ctx has no route info")
	}
}

// TestAccountNegatives_BYOKDoesNotLeakUserJWT verifies that when
// BYOK=true, the UserJWT in the route info is NEVER selected as the
// key. This is a security boundary : the user's identity token must
// not be sent to a third-party provider.
func TestAccountNegatives_BYOKDoesNotLeakUserJWT(t *testing.T) {
	acc := newAccount(Config{})
	ctx := context.WithValue(context.Background(), ctxKeyRoute, &routeInfo{
		BYOK:    true,
		APIKey:  "sk-ant-real-key",
		UserJWT: "this-jwt-must-not-leak-to-anthropic",
	})
	keys, err := acc.GetKeysForProvider(ctx, schemas.Anthropic)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys: %d", len(keys))
	}
	k := keys[0]
	if k.Value.Val != "sk-ant-real-key" {
		t.Errorf("BYOK path selected the wrong value: %q", k.Value.Val)
	}
	if k.Value.Val == "this-jwt-must-not-leak-to-anthropic" {
		t.Fatal("SECURITY : UserJWT leaked to direct provider path")
	}
	if k.ID != "direct" {
		t.Errorf("BYOK key.ID should be 'direct', got %q", k.ID)
	}
}

// TestAccountNegatives_GatewayUsesUserJWTNotAPIKey : symmetric to the
// previous test. In gateway mode, the gateway is digitorn-internal and
// expects our UserJWT as bearer ; any APIKey in the route info must be
// IGNORED (it's there only for BYOK).
func TestAccountNegatives_GatewayUsesUserJWTNotAPIKey(t *testing.T) {
	acc := newAccount(Config{})
	ctx := context.WithValue(context.Background(), ctxKeyRoute, &routeInfo{
		BYOK:    false,
		UserJWT: "user-jwt-token-here",
		APIKey:  "sk-this-should-not-be-used",
	})
	keys, err := acc.GetKeysForProvider(ctx, schemas.OpenAI)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys: %d", len(keys))
	}
	if keys[0].Value.Val != "user-jwt-token-here" {
		t.Errorf("gateway path selected wrong value: %q", keys[0].Value.Val)
	}
	if keys[0].ID != "gateway" {
		t.Errorf("gateway key.ID should be 'gateway', got %q", keys[0].ID)
	}
}

// TestAccountNegatives_ResolveProvider_GatewayWhenNotBYOK : the
// ResolveProvider function must route through OpenAI (gateway adapter)
// whenever BYOK=false, regardless of the requested provider string.
// The gateway speaks OpenAI-compatible REST internally.
func TestAccountNegatives_ResolveProvider_GatewayWhenNotBYOK(t *testing.T) {
	for _, name := range []string{"anthropic", "openai", "cohere", "mistral", "groq", "fireworks", "ollama"} {
		got := ResolveProvider(&llm.ChatRequest{BYOK: false, Provider: name})
		if got != schemas.OpenAI {
			t.Errorf("BYOK=false, Provider=%q → %s ; expected OpenAI gateway", name, got)
		}
	}
}

// TestAccountNegatives_ResolveProvider_BYOKHonoursProviderName : when
// BYOK=true, the provider string MUST be honored so the right native
// API is called.
func TestAccountNegatives_ResolveProvider_BYOKHonoursProviderName(t *testing.T) {
	cases := []struct {
		name string
		want schemas.ModelProvider
	}{
		{"anthropic", schemas.Anthropic},
		{"openai", schemas.OpenAI},
		{"cohere", schemas.Cohere},
		{"mistral", schemas.Mistral},
		{"gemini", schemas.Gemini},
		{"ollama", schemas.Ollama},
		{"xai", schemas.XAI},
		{"grok", schemas.XAI},
	}
	for _, c := range cases {
		got := ResolveProvider(&llm.ChatRequest{BYOK: true, Provider: c.name})
		if got != c.want {
			t.Errorf("BYOK Provider=%q → %s ; want %s", c.name, got, c.want)
		}
	}
}

// TestAccountNegatives_GetConfiguredProviders_LongerThanZero ensures
// the account declares at least the main providers — defence against
// a regression that silently empties the list.
func TestAccountNegatives_GetConfiguredProviders_LongerThanZero(t *testing.T) {
	acc := newAccount(Config{})
	provs, err := acc.GetConfiguredProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) < 5 {
		t.Errorf("only %d providers configured ; expected at least 5", len(provs))
	}
	wantSet := map[schemas.ModelProvider]bool{
		schemas.OpenAI: true, schemas.Anthropic: true, schemas.Gemini: true,
	}
	for _, p := range provs {
		delete(wantSet, p)
	}
	if len(wantSet) > 0 {
		t.Errorf("missing required providers: %v", wantSet)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Silence "unused" complaint for errors import if test doesn't use it.
var _ = errors.New
