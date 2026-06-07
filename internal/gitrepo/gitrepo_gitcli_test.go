package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests cross-validate our go-git output against the REAL git binary on
// the very same shadow repo — the strongest correctness guarantee we can get.

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func runGit(t *testing.T, gitDir, workdir string, args ...string) string {
	t.Helper()
	full := append([]string{"--git-dir", gitDir, "--work-tree", workdir}, args...)
	cmd := exec.Command("git", full...)
	// Isolate from the operator's global git config (incl. safe.directory).
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func parsePorcelain(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		code := line[:2]
		path := strings.TrimSpace(line[3:])
		path = strings.Trim(path, `"`)
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		var st string
		switch {
		case code == "??" || strings.Contains(code, "A"):
			st = "added"
		case strings.Contains(code, "D"):
			st = "deleted"
		default:
			st = "modified"
		}
		m[path] = st
	}
	return m
}

func TestParity_StatusMatchesGitCLI(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	r, dir := fresh(t)
	writeFile(t, dir, "keep.txt", "k1\nk2\n")
	writeFile(t, dir, "mod.txt", "m1\nm2\n")
	commitAll(t, r, "base")
	// added / modified / deleted in one shot
	writeFile(t, dir, "sub/new.txt", "new\n")
	writeFile(t, dir, "mod.txt", "m1\nCHANGED\n")
	if err := os.Remove(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}

	ours := changeSet(t, r)
	gitDir := filepath.Join(dir, ".digitorn", "git")
	// -uall: list untracked FILES individually (git's default collapses an
	// untracked dir to "sub/"; go-git reports per file, which is what the
	// Monaco UI needs — so we compare at file granularity).
	theirs := parsePorcelain(runGit(t, gitDir, dir, "status", "--porcelain", "--untracked-files=all"))

	if len(ours) != len(theirs) {
		t.Fatalf("file-set size mismatch\n ours=%v\ntheirs=%v", ours, theirs)
	}
	for p, st := range ours {
		if theirs[p] != st {
			t.Fatalf("status mismatch for %q: ours=%s git=%s\n ours=%v\ntheirs=%v", p, st, theirs[p], ours, theirs)
		}
	}
	// And git must NOT see our .digitorn metadata.
	for p := range theirs {
		if strings.HasPrefix(p, ".digitorn") {
			t.Fatalf("git CLI saw .digitorn metadata (info/exclude not honoured): %q", p)
		}
	}
}

func parseNumstat(out string) (ins, del int) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		i, e1 := strconv.Atoi(f[0])
		d, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil {
			ins += i
			del += d
		}
	}
	return ins, del
}

func TestParity_NumstatMatchesGitCLI(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	cases := []struct{ name, before, after string }{
		{"midchange_and_append", "a\nb\nc\nd\n", "a\nB\nc\nd\nE\n"},
		{"delete_lines", "1\n2\n3\n4\n5\n", "1\n5\n"},
		{"no_trailing_newline", "x\ny\nz\n", "x\nY\nz"},
		{"full_rewrite", "old1\nold2\n", "new1\nnew2\nnew3\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, dir := fresh(t)
			writeFile(t, dir, "f.txt", tc.before)
			commitAll(t, r, "base")
			writeFile(t, dir, "f.txt", tc.after)

			_, ins, del, err := r.FileDiff("f.txt")
			if err != nil {
				t.Fatal(err)
			}
			gitDir := filepath.Join(dir, ".digitorn", "git")
			gi, gd := parseNumstat(runGit(t, gitDir, dir, "diff", "--numstat", "--", "f.txt"))
			if ins != gi || del != gd {
				t.Fatalf("numstat mismatch: ours=%d/%d git=%d/%d", ins, del, gi, gd)
			}
		})
	}
}
