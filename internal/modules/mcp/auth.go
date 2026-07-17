package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/pkg/module"
)

func (m *Module) applyServerAuth(spec connectSpec, serverID string, sc schema.MCPServerConfig, ce catalogEntry, ac module.AuthContext) connectSpec {
	if normTransport(spec.Transport) != "stdio" {
		return spec
	}
	if ac.Token == "" && ac.ClientID == "" {
		return spec
	}

	provider := firstNonEmpty(ac.Provider, ce.OAuthProvider)

	if provider == "google" && ac.ClientID != "" && ac.ClientSecret != "" {
		if env, ok := writeGoogleKeyfile(serverID, ce, ac); ok {
			spec.Env = mergeEnv(spec.Env, env)
			spec.AuthFP = authFingerprint(ac)
			return spec
		}
	}

	envVar := firstNonEmpty(ac.EnvTokenVar, ce.OAuthEnvTokenVar)
	if envVar == "" && sc.Auth != nil {
		envVar = sc.Auth.EnvTokenVar
	}
	if envVar != "" && ac.Token != "" {
		spec.Env = mergeEnv(spec.Env, map[string]string{envVar: ac.Token})
		spec.AuthFP = authFingerprint(ac)
	}
	return spec
}

func writeGoogleKeyfile(serverID string, ce catalogEntry, ac module.AuthContext) (map[string]string, bool) {
	keyEnv := ce.OAuthKeyfileEnv
	credEnv := ce.OAuthCredentialsEnv
	if keyEnv == "" || credEnv == "" {
		return nil, false
	}
	credFile := firstNonEmpty(ce.OAuthCredentialsFilename, ".credentials.json")

	dir := serverAuthDir(serverID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false
	}
	keyPath := filepath.Join(dir, "gcp-oauth.keys.json")
	keys := map[string]any{"installed": map[string]any{
		"client_id":     ac.ClientID,
		"client_secret": ac.ClientSecret,
		"redirect_uris": []string{"http://localhost"},
		"auth_uri":      "https://accounts.google.com/o/oauth2/auth",
		"token_uri":     "https://oauth2.googleapis.com/token",
	}}
	if err := writeJSONFile0600(keyPath, keys); err != nil {
		return nil, false
	}

	credPath := filepath.Join(dir, credFile)
	if ac.Token != "" {
		creds := map[string]any{
			"type":          "authorized_user",
			"access_token":  ac.Token,
			"refresh_token": ac.RefreshToken,
			"scope":         ac.Scope,
			"token_type":    firstNonEmpty(ac.TokenType, "Bearer"),
			"expiry_date":   expiryUnixMillis(ac.ExpiresAt),
		}
		_ = writeJSONFile0600(credPath, creds)
	}
	return map[string]string{keyEnv: keyPath, credEnv: credPath}, true
}

func serverAuthDir(serverID string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".digitorn", "mcp", "servers", serverID)
}

func writeJSONFile0600(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func expiryUnixMillis(sec int64) int64 {
	if sec <= 0 {
		return 0
	}
	return sec * 1000
}

func authFingerprint(ac module.AuthContext) string {
	h := sha256.Sum256([]byte(ac.Token + "\x00" + ac.RefreshToken + "\x00" + ac.ClientID))
	return hex.EncodeToString(h[:8])
}

func mergeEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
