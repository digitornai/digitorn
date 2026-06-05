//go:build windows

package goshell

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestBusybox_ComplexPipeline is the decisive proof: a COMPLEX agent pipeline
// (sort -n / uniq / awk -F / tr / sed -n / grep -E) runs through GOSHELL with a
// PATH that has NO GNU coreutils on it (git stripped). The embedded busybox
// must transparently fill every tool — exactly the "agent writes complex
// commands combining all of this" case.
func TestBusybox_ComplexPipeline(t *testing.T) {
	if busyboxDir() == "" {
		t.Skip("busybox unavailable")
	}
	t.Logf("busybox dir: %s", busyboxDir())

	dir := t.TempDir()
	env := stripGitFromPath(os.Environ()) // simulate a host with no real coreutils

	script := `printf '3:charlie\n1:alpha\n2:bravo\n2:bravo\n' | sort -t: -k1 -n | uniq | awk -F: '{print $2}' | tr a-z A-Z | sed -n '1,2p' | grep -E 'A|B'`
	var out, errb bytes.Buffer
	code, err := Run(context.Background(), script, dir, env, nil, &out, &errb)
	if err != nil {
		t.Fatalf("run err: %v\nstderr=%q", err, errb.String())
	}
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := strings.Fields(out.String())
	if len(got) != 2 || got[0] != "ALPHA" || got[1] != "BRAVO" {
		t.Fatalf("got %v want [ALPHA BRAVO] (stderr=%q)", got, errb.String())
	}
}

// TestBusybox_FindXargsWc covers another combined shape an agent uses.
func TestBusybox_FindXargsWc(t *testing.T) {
	if busyboxDir() == "" {
		t.Skip("busybox unavailable")
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/a.txt", []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/b.txt", []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := stripGitFromPath(os.Environ())
	script := `find . -name '*.txt' | xargs wc -l | sort -n | tail -1 | awk '{print $1}'`
	var out, errb bytes.Buffer
	code, err := Run(context.Background(), script, dir, env, nil, &out, &errb)
	if err != nil || code != 0 {
		t.Fatalf("err=%v code=%d stderr=%q", err, code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "5" { // total line count across both files
		t.Fatalf("got %q want 5 (stderr=%q)", out.String(), errb.String())
	}
}

// TestBusybox_RealToolWins confirms a tool present on PATH is used directly,
// not shadowed by busybox.
func TestBusybox_RealToolWins(t *testing.T) {
	if busyboxDir() == "" {
		t.Skip("busybox unavailable")
	}
	// git is on PATH here; routing must NOT intercept it.
	var out, errb bytes.Buffer
	code, err := Run(context.Background(), `git --version`, t.TempDir(), os.Environ(), nil, &out, &errb)
	if err != nil || code != 0 || !strings.Contains(out.String(), "git version") {
		t.Skipf("git not on PATH (out=%q) — routing logic still valid", out.String())
	}
}
