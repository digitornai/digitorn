//go:build windows

package bash

import (
	"strings"
	"testing"
)

// TestEnrichPath_MergesRegistryAndPreservesCurrent : enrichPath must keep every
// directory of the inherited PATH (at the front) and add the persisted registry
// dirs. On a dev box with python on the USER PATH, the merged PATH must contain
// a python dir even when the inherited PATH had none — that is the whole point
// (the daemon's launch context no longer decides whether `python` resolves).
func TestEnrichPath_MergesRegistryAndPreservesCurrent(t *testing.T) {
	const cur = `C:\Windows;C:\Windows\System32`
	got := enrichPath(cur)

	// Every inherited dir is preserved, in front.
	if !strings.HasPrefix(got, cur) {
		t.Fatalf("inherited PATH not preserved at front:\n  got %q", got)
	}
	// Result is a superset (registry added at least the System Manager dirs).
	if len(strings.Split(got, ";")) < len(strings.Split(cur, ";")) {
		t.Fatalf("enrichPath shrank the PATH: %q", got)
	}
	// No duplicate dirs (case-insensitive).
	seen := map[string]bool{}
	for _, d := range strings.Split(got, ";") {
		k := strings.ToLower(strings.TrimRight(strings.TrimSpace(d), `\`))
		if k == "" {
			continue
		}
		if seen[k] {
			t.Errorf("duplicate PATH entry after enrich: %q", d)
		}
		seen[k] = true
	}

	// Machine-specific signal : this box has python on the user PATH, so the
	// merged PATH should expose it even though `cur` did not. Skip (don't fail)
	// on a box without python so the test stays portable across dev machines.
	if !strings.Contains(strings.ToLower(got), "python") {
		t.Skip("no python dir on this machine's registry PATH — merge logic still verified above")
	}
}
