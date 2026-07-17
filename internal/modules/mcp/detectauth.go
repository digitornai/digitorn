package mcp

import (
	"slices"
	"strings"
)

var oauthEnvPrefixes = map[string]string{
	"google": "google", "github": "github", "notion": "notion",
	"slack": "slack", "microsoft": "microsoft", "azure": "microsoft",
	"spotify": "custom", "dropbox": "custom", "discord": "custom",
	"twitter": "custom", "facebook": "custom", "linkedin": "custom",
}

type detectedAuth struct {
	Provider        string
	ClientIDVar     string
	ClientSecretVar string
	TokenVar        string
}

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
		return nil
	}
	return &detectedAuth{
		Provider:        providerFromVar(clientID),
		ClientIDVar:     clientID,
		ClientSecretVar: clientSecret,
		TokenVar:        detectTokenVar(names),
	}
}

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

func envVarToShorthands(name string) []string {
	low := strings.ToLower(name)
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if strings.HasSuffix(low, "access_token") || strings.HasSuffix(low, "api_token") || low == "token" {
		add("token")
	}
	if strings.HasSuffix(low, "api_key") {
		add("api_key")
	}
	if _, rest, ok := strings.Cut(low, "_"); ok {
		add(rest)
	}
	add(low)
	return out
}

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
