package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const CredentialsFile = ".digitorn/credentials.json"

const CredentialsEnv = "DIGITORN_DEV_JWT"

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
			return nil, nil
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

func CredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credentials: home dir: %w", err)
	}
	return filepath.Join(home, CredentialsFile), nil
}

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

func (c *Credentials) IsExpired(leeway time.Duration) bool {
	if c == nil || c.ExpiresAt == 0 {
		return false
	}
	exp := time.Unix(int64(c.ExpiresAt), 0)
	return time.Now().Add(leeway).After(exp)
}

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
