package mcp

import (
	"slices"
	"strings"
)

// oauthEnvPrefixes maps a known vendor prefix (lower-case, taken from the
// *_CLIENT_ID env var) to its OAuth provider. Anything unmatched → "custom".
var oauthEnvPrefixes = map[string]string{
	"google": "google", "github": "github", "notion": "notion",
	"slack": "slack", "microsoft": "microsoft", "azure": "microsoft",
	"spotify": "custom", "dropbox": "custom", "discord": "custom",
	"twitter": "custom", "facebook": "custom", "linkedin": "custom",
}

// detectedAuth is an auth config inferred purely from a server's declared env
// var NAMES — no per-server hardcoding, so an unknown server auto-configures.
type detectedAuth struct {
	Provider        string
	ClientIDVar     string
	ClientSecretVar string
	TokenVar        string
}

// detectOAuthFromEnvVars infers a server's auth from the NAMES of the env vars
// it declares. OAuth is only inferred when BOTH a client-id AND a client-secret
// var are present (a lone token var is not OAuth — it stays a plain token).
// Mirrors the old daemon's _detect_oauth_from_env_vars so any server's auth is
// auto-derived. Returns nil when nothing auth-like is found.
func detectOAuthFromEnvVars(names []string) *detectedAuth {
	if len(names) == 0 {
		return nil
	}
	var clientID, clientSecret string
	for _, raw := range names {
		n := strings.ToUpper(raw)
		switch {
		case strings.Contains(n, "CLIENT_ID"):
			if clientID == "" {
				clientID = raw
			}
		case strings.Contains(n, "CLIENT_SECRET"):
			if clientSecret == "" {
				clientSecret = raw
			}
		}
	}
	if clientID == "" || clientSecret == "" {
		return nil // OAuth needs both; a token-only server isn't OAuth
	}
	return &detectedAuth{
		Provider:        providerFromVar(clientID),
		ClientIDVar:     clientID,
		ClientSecretVar: clientSecret,
		TokenVar:        detectTokenVar(names),
	}
}

// detectTokenVar returns the single credential env var a server declares for a
// plain bearer token / API key — independent of OAuth, so the common
// single-token server is covered too. Empty when none is declared.
func detectTokenVar(names []string) string {
	for _, raw := range names {
		n := strings.ToUpper(raw)
		switch {
		case strings.Contains(n, "ACCESS_TOKEN"), strings.Contains(n, "API_TOKEN"):
			return raw
		case strings.HasSuffix(n, "_TOKEN") && !strings.Contains(n, "BOT"):
			return raw
		case strings.HasSuffix(n, "_API_KEY"):
			return raw
		}
	}
	return ""
}

// providerFromVar derives the OAuth provider from a *_CLIENT_ID var name (the
// prefix before CLIENT_ID), matched against the known-vendor table.
func providerFromVar(clientIDVar string) string {
	up := strings.ToUpper(clientIDVar)
	prefix := strings.Trim(strings.SplitN(up, "CLIENT_ID", 2)[0], "_")
	low := strings.ToLower(prefix)
	for vendor, provider := range oauthEnvPrefixes {
		if strings.HasPrefix(low, vendor) {
			return provider
		}
	}
	return "custom"
}

// envVarToShorthands returns the candidate shorthands a user might write for a
// declared env var, most-specific first, so the mapping is lenient: a server's
// GITHUB_PERSONAL_ACCESS_TOKEN matches a user's `token` OR `access_token` OR the
// full lowercase name. No per-server hardcoding.
func envVarToShorthands(name string) []string {
	low := strings.ToLower(name)
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	// token-family vars collapse to the universal `token`.
	if strings.HasSuffix(low, "access_token") || strings.HasSuffix(low, "api_token") || low == "token" {
		add("token")
	}
	if strings.HasSuffix(low, "api_key") {
		add("api_key")
	}
	if _, rest, ok := strings.Cut(low, "_"); ok { // after the first underscore
		add(rest)
	}
	add(low) // the full name as a last resort
	return out
}

// tokenishValue returns the first token-like value the user supplied under any
// common shorthand — used to inject a credential into a server's detected token
// env var without the user having to know its exact name.
func tokenishValue(extra map[string]any) string {
	for _, k := range []string{"token", "api_key", "apikey", "access_token", "key", "secret", "bot_token", "pat"} {
		if v, ok := extra[k]; ok {
			if s, _ := v.(string); s != "" {
				return s
			}
		}
	}
	return ""
}
