// Package mcpoauth owns the daemon-side MCP OAuth token store, encryption, and
// authorization flow. Tokens are resolved per (user, provider) and injected into
// MCP server calls by the daemon; the worker never persists secrets.
package mcpoauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/secretbox"
)

// Sealer encrypts secrets at rest with a process-wide key loaded from disk.
type Sealer struct {
	key [32]byte
}

func NewSealer(keyPath string) (*Sealer, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &Sealer{key: key}, nil
}

func (s *Sealer) Seal(plaintext []byte) (string, error) {
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nonce[:], plaintext, &nonce, &s.key)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Sealer) Open(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(raw) < 24 {
		return nil, fmt.Errorf("mcpoauth: ciphertext too short")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	out, ok := secretbox.Open(nil, raw[24:], &nonce, &s.key)
	if !ok {
		return nil, fmt.Errorf("mcpoauth: decrypt failed")
	}
	return out, nil
}

// loadOrCreateKey reads a base64 32-byte key, or generates one and writes it
// atomically. O_EXCL guards the create against a concurrent process — on a lost
// race it reads the winner's key back.
func loadOrCreateKey(path string) ([32]byte, error) {
	var key [32]byte
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := base64.StdEncoding.DecodeString(string(b))
		if derr != nil || len(raw) != 32 {
			return key, fmt.Errorf("mcpoauth: malformed key file %s", path)
		}
		copy(key[:], raw)
		return key, nil
	}
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return key, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return key, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return loadOrCreateKey(path)
		}
		return key, err
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key[:])); err != nil {
		return key, err
	}
	return key, nil
}

// DefaultKeyPath is {USER_HOME}/.digitorn/server.key.
func DefaultKeyPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".digitorn", "server.key")
	}
	return filepath.Join(".digitorn", "server.key")
}
