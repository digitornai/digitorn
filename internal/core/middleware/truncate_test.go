package middleware

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateUTF8_NeverSplitsRune cuts a multi-byte string at EVERY possible
// byte length and asserts the result is always valid UTF-8 and never longer
// than the requested max — the regression for the rune-splitting truncation
// bug (a raw s[:max] emits invalid bytes mid-rune).
func TestTruncateUTF8_NeverSplitsRune(t *testing.T) {
	// Mix of 1/2/3/4-byte runes : ASCII, é (2B), € (3B), 😀 (4B).
	s := "aé€😀bçü日本語🚀z"
	for max := 0; max <= len(s)+2; max++ {
		got := truncateUTF8(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8: %q", max, got)
		}
		if len(got) > max && max >= 0 {
			t.Fatalf("max=%d : result %q is %d bytes, exceeds max", max, got, len(got))
		}
	}
}

func TestTruncateUTF8_ShortStringUnchanged(t *testing.T) {
	if got := truncateUTF8("héllo", 100); got != "héllo" {
		t.Fatalf("short string altered: %q", got)
	}
	if got := truncateUTF8("x", 0); got != "" {
		t.Fatalf("max=0 must be empty, got %q", got)
	}
}
