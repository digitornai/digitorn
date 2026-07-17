package sessionstore

import (
	"os"
	"strings"
	"testing"
)

func TestEncodeSessionDir_IdentityForNormalSids(t *testing.T) {
	for _, sid := range []string{
		"bd79cee8-c650-472f-b444-9c7cf7127680",
		"a1",
		"user_123",
		"sess.42",
		"abc#def~ghi",
	} {
		if got := encodeSessionDir(sid); got != sid {
			t.Errorf("normal sid %q must encode to itself, got %q (would migrate existing dirs)", sid, got)
		}
		if got := decodeSessionDir(sid); got != sid {
			t.Errorf("normal sid %q must decode to itself, got %q", sid, got)
		}
	}
}

func TestEncodeSessionDir_EscapesSubAgentColons(t *testing.T) {
	sid := "bd79cee8-c650-472f-b444-9c7cf7127680::agent::researcher#a396c996de"
	enc := encodeSessionDir(sid)
	if strings.ContainsAny(enc, `:<>"/\|?*`) {
		t.Fatalf("encoded dir name still contains an OS-illegal char: %q", enc)
	}
	if !strings.Contains(enc, "%3A") {
		t.Errorf("':' should be escaped as %%3A, got %q", enc)
	}
	if dec := decodeSessionDir(enc); dec != sid {
		t.Errorf("round-trip failed: %q -> %q -> %q", sid, enc, dec)
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	for _, sid := range []string{
		"x",
		"a:b",
		"root::agent::researcher#abc",
		"root::agent::w#1::agent::deep#2",
		"already has %25 percent",
		"héllo-wörld",
		"trailing:",
		":leading",
	} {
		enc := encodeSessionDir(sid)
		if strings.ContainsAny(enc, `:<>"/\|?*`) {
			t.Errorf("encoded %q -> %q still has an OS-illegal char", sid, enc)
		}
		if got := decodeSessionDir(enc); got != sid {
			t.Errorf("round-trip mismatch for %q: enc=%q dec=%q", sid, enc, got)
		}
	}
}

func TestSessionDir_SubAgent_MkdirSucceeds(t *testing.T) {
	p := NewPaths(t.TempDir())
	sid := "bd79cee8-c650-472f-b444-9c7cf7127680::agent::researcher#a396c996de"

	dir := p.SessionDir(sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sub-agent session dir failed (the live bug): %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("sub-agent session dir not created: %v", err)
	}

	other := "bd79cee8-c650-472f-b444-9c7cf7127680::agent::writer#deadbeef00"
	if p.SessionDir(other) == dir {
		t.Fatal("distinct sub-agent sids collided on one directory")
	}

	leaf := encodeSessionDir(sid)
	if strings.ContainsAny(leaf, `:<>"/\|?*`) {
		t.Errorf("session dir leaf %q contains an OS-illegal character", leaf)
	}
}
