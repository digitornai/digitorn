package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CredentialsFile is the standard path the digitorn auth flow writes
// the access token to. Same convention as the Python daemon's
// credentials store — exists when the user has logged in via
// auth.digitorn.ai (the `digitorn login` command will populate it in
// CLI-7 when we wire OAuth).
const CredentialsFile = ".digitorn/credentials.json"

// CredentialsEnv is an escape hatch : if set, the value is treated as
// the bearer JWT directly. Useful for CI / scripts / cases where the
// user has fetched a token through a non-standard flow.
const CredentialsEnv = "DIGITORN_DEV_JWT"

// Credentials is the on-disk shape written by the auth flow. Only
// AccessToken is required ; the rest is metadata for UX (display the
// signed-in email in the status bar).
type Credentials struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token,omitempty"`
	ExpiresAt    float64 `json:"expires_at,omitempty"`
	AuthURL      string  `json:"auth_url,omitempty"`
	Provider     string  `json:"provider,omitempty"`
	UserID       string  `json:"user_id,omitempty"`
	Email        string  `json:"email,omitempty"`
	Name         string  `json:"name,omitempty"`
}

// LoadCredentials returns the token + metadata, looking up sources in
// this precedence :
//
//  1. DIGITORN_DEV_JWT env var (token-only ; UserID + Email left empty)
//  2. $HOME/.digitorn/credentials.json (full Credentials struct)
//
// Returns (nil, nil) when neither source has anything — daemon dev
// mode accepts unauthenticated requests, so empty is a valid state.
// Returns an error only on actual file/parse failures.
func LoadCredentials() (*Credentials, error) {
	if tok := os.Getenv(CredentialsEnv); tok != "" {
		return &Credentials{AccessToken: tok, Provider: "env"}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("credentials: home dir: %w", err)
	}
	path := filepath.Join(home, CredentialsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // unauthenticated is OK
		}
		return nil, fmt.Errorf("credentials: read %s: %w", path, err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("credentials: decode %s: %w", path, err)
	}
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("credentials: %s missing access_token", path)
	}
	return &creds, nil
}

// CredentialsPath returns the absolute path the auth flow writes to. The
// directory may not exist yet ; SaveCredentials creates it as needed.
func CredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credentials: home dir: %w", err)
	}
	return filepath.Join(home, CredentialsFile), nil
}

// SaveCredentials writes creds to ~/.digitorn/credentials.json with 0600
// permissions and atomic rename. Creates the parent dir if missing.
func SaveCredentials(creds *Credentials) error {
	if creds == nil || creds.AccessToken == "" {
		return errors.New("credentials: refusing to save empty creds")
	}
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("credentials: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("credentials: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("credentials: rename: %w", err)
	}
	return nil
}

// DeleteCredentials removes the credentials file. Returns nil if it
// didn't exist — logout is idempotent.
func DeleteCredentials() error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credentials: remove: %w", err)
	}
	return nil
}

// IsExpired returns true when the access token's recorded expiry is in
// the past (or within the given leeway). Returns false if no expiry is
// recorded — we can't tell, assume valid.
func (c *Credentials) IsExpired(leeway time.Duration) bool {
	if c == nil || c.ExpiresAt == 0 {
		return false
	}
	exp := time.Unix(int64(c.ExpiresAt), 0)
	return time.Now().Add(leeway).After(exp)
}

// DefaultUserID returns the user_id to send in the X-User-ID header
// when no full JWT is available. Pulls from the credentials struct if
// present, else from $USER / $USERNAME, else "anonymous". This lets
// the CLI work in dev-mode daemon (auth.enabled=false) without
// requiring a full token.
func DefaultUserID(creds *Credentials) string {
	if creds != nil && creds.UserID != "" {
		return creds.UserID
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "anonymous"
}
