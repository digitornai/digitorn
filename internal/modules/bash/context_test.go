package bash

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func newGoShellModule(t *testing.T) *Module {
	t.Helper()
	m := New()
	m.cfg.Workdir = t.TempDir()
	m.cfg.Shell = "goshell"
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	return m
}

func runCmd(t *testing.T, m *Module, cmd string) runResult {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"command": cmd})
	res, err := m.run(context.Background(), raw)
	if err != nil {
		t.Fatalf("%q: err %v", cmd, err)
	}
	var d runResult
	data, _ := json.Marshal(res.Data)
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// TestEnrich_FilesAndDuration: a command that writes a file reports it in
// files_changed, a read-only command reports none, and a command that spends
// real time reports a non-zero duration.
func TestEnrich_FilesAndDuration(t *testing.T) {
	m := newGoShellModule(t)

	d := runCmd(t, m, `echo hello > out.txt`)
	if !containsStr(d.FilesChanged, "out.txt") {
		t.Fatalf("files_changed missing out.txt: %v", d.FilesChanged)
	}

	d = runCmd(t, m, `echo just-reading`)
	if len(d.FilesChanged) != 0 {
		t.Fatalf("read-only command reported files: %v", d.FilesChanged)
	}

	// A busy loop spends measurable wall time → duration must be surfaced.
	d = runCmd(t, m, `i=0; while [ $i -lt 200000 ]; do i=$((i+1)); done; echo done`)
	if d.DurationMs <= 0 {
		t.Fatalf("duration_ms not reported for a slow command: %d", d.DurationMs)
	}
}

// TestEnrich_BoundedScan: when a command creates more than the cap, the list is
// bounded AND a note says so — never a silent "looks complete".
func TestEnrich_BoundedScan(t *testing.T) {
	m := newGoShellModule(t)
	d := runCmd(t, m, `i=0; while [ $i -lt 60 ]; do echo x > "f$i.txt"; i=$((i+1)); done`)
	if len(d.FilesChanged) > filesScanCap {
		t.Fatalf("list not capped: %d files", len(d.FilesChanged))
	}
	if d.FilesNote == "" {
		t.Fatalf("60 files changed but no bounding note")
	}
}

// TestEnrich_Git: inside a real repo, the branch and dirty counts ride along —
// and the dirty count is cache-served on a read-only command, recomputed only
// when files actually change (the perf contract).
func TestEnrich_Git(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	m := newGoShellModule(t)
	root := m.cfg.Workdir
	gitInit(t, root)

	d := runCmd(t, m, `echo content > tracked.txt`)
	if d.Git == nil {
		t.Fatalf("no git context inside a repo")
	}
	if d.Git.Branch == "" {
		t.Fatalf("branch not detected")
	}
	if d.Git.Untracked < 1 {
		t.Fatalf("new file not counted as untracked: %+v", d.Git)
	}

	// Perf contract: create a file OUT of band, then a read-only command must
	// serve the STALE cached count (proving git status wasn't re-run), while a
	// file-changing command refreshes it.
	first := m.gitContext(root, true).Untracked
	if err := os.WriteFile(root+"/sneaky.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cached := m.gitContext(root, false).Untracked; cached != first {
		t.Fatalf("read-only path recomputed git (cache miss): got %d want cached %d", cached, first)
	}
	if fresh := m.gitContext(root, true).Untracked; fresh != first+1 {
		t.Fatalf("file-changing path did not refresh git: got %d want %d", fresh, first+1)
	}
}

// TestTerminalSnapshot: the host snapshot is computed and baked into the run
// tool's description, so the agent knows its terrain from turn one.
func TestTerminalSnapshot(t *testing.T) {
	m := newGoShellModule(t)
	if !strings.Contains(m.envInfo, "OS ") || !strings.Contains(m.envInfo, "shell:") {
		t.Fatalf("env snapshot incomplete: %q", m.envInfo)
	}
	var desc string
	for _, tl := range m.Tools() {
		if tl.Name == "run" {
			desc = tl.Description
		}
	}
	if !strings.Contains(desc, "This host:") {
		t.Fatalf("run description missing host snapshot:\n%s", desc)
	}
	if strings.Contains(desc, "persistent per session") {
		t.Fatalf("stale persistent-shell claim still in description")
	}
}

// TestContextDisabled: DIGITORN_BASH_CONTEXT=0 turns enrichment off entirely.
func TestContextDisabled(t *testing.T) {
	t.Setenv("DIGITORN_BASH_CONTEXT", "0")
	m := newGoShellModule(t)
	d := runCmd(t, m, `echo hi > x.txt`)
	if len(d.FilesChanged) != 0 || d.DurationMs != 0 || d.Git != nil {
		t.Fatalf("context not disabled: files=%v dur=%d git=%v", d.FilesChanged, d.DurationMs, d.Git)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t.t"},
		{"config", "user.name", "t"},
		{"checkout", "-q", "-b", "main"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
