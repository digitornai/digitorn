package mcpoauth

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSealer_RoundTrip(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server.key")
	s, err := NewSealer(keyPath)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	plain := []byte(`{"access_token":"abc","refresh_token":"def"}`)
	sealed, err := s.Seal(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed == string(plain) {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := s.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestSealer_OpenRejectsTampered(t *testing.T) {
	s, err := NewSealer(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	sealed, _ := s.Seal([]byte("secret"))
	if _, err := s.Open(sealed + "x"); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
	if _, err := s.Open("not-base64!!"); err == nil {
		t.Fatal("expected error on bad base64")
	}
	if _, err := s.Open("c2hvcnQ="); err == nil {
		t.Fatal("expected error on short ciphertext")
	}
}

func TestLoadOrCreateKey_StableAndPersisted(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "sub", "server.key")
	a, err := loadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	b, err := loadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a != b {
		t.Fatal("key changed on reload")
	}
}

func TestLoadOrCreateKey_RejectsMalformed(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server.key")
	if err := os.WriteFile(keyPath, []byte("not-a-valid-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateKey(keyPath); err == nil {
		t.Fatal("expected error on malformed key file")
	}
}
