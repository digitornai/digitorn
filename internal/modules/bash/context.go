package bash

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/modules/bash/goshell"
)

func (m *Module) enrich(res *cmdResult, root string, started time.Time) {
	if !m.collectCtx {
		return
	}
	res.DurationMs = time.Since(started).Milliseconds()
	if root == "" {
		return
	}
	if res.Cwd == "" {
		res.Cwd = root
	}
	res.FilesChanged, res.FilesNote = filesChangedSince(root, started)
	res.Git = m.gitContext(root, len(res.FilesChanged) > 0)
}

type gitInfo struct {
	Branch    string `json:"branch,omitempty"`
	Staged    int    `json:"staged,omitempty"`
	Modified  int    `json:"modified,omitempty"`
	Untracked int    `json:"untracked,omitempty"`
}

var dirsToSkip = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "venv": true,
	"vendor": true, "dist": true, "build": true, "target": true,
	".next": true, "__pycache__": true, ".idea": true, ".gradle": true,
	".cache": true, "obj": true, ".pytest_cache": true, ".mypy_cache": true,
}

const (
	filesScanCap     = 50
	filesScanBudget  = 150 * time.Millisecond
	gitStatusTimeout = 400 * time.Millisecond
)

func filesChangedSince(root string, since time.Time) ([]string, string) {
	if root == "" {
		return nil, ""
	}
	start := time.Now()
	var files []string
	var note string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && dirsToSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if time.Since(start) > filesScanBudget {
			note = "scan time-bounded; list may be incomplete"
			return filepath.SkipAll
		}
		info, e := d.Info()
		if e != nil {
			return nil
		}
		if !info.ModTime().Before(since) {
			rel, e := filepath.Rel(root, p)
			if e != nil {
				rel = p
			}
			files = append(files, filepath.ToSlash(rel))
			if len(files) >= filesScanCap {
				note = "more than " + itoa(filesScanCap) + " files changed; list bounded"
				return filepath.SkipAll
			}
		}
		return nil
	})
	return files, note
}

func (m *Module) gitContext(root string, changed bool) *gitInfo {
	gitRoot := findGitDir(root)
	if gitRoot == "" {
		return nil
	}
	branch := gitBranch(gitRoot)

	m.gitMu.Lock()
	cached := m.gitCache[root]
	m.gitMu.Unlock()
	if cached != nil && !changed {
		c := *cached
		c.Branch = branch
		return &c
	}

	g := &gitInfo{Branch: branch}
	gitDirtyCounts(root, g)

	m.gitMu.Lock()
	if m.gitCache == nil {
		m.gitCache = map[string]*gitInfo{}
	}
	m.gitCache[root] = g
	m.gitMu.Unlock()
	cp := *g
	return &cp
}

func findGitDir(start string) string {
	dir := start
	for i := 0; i < 40 && dir != ""; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func gitBranch(repoRoot string) string {
	b, err := os.ReadFile(filepath.Join(repoRoot, ".git", "HEAD"))
	if err != nil {
		return gitBranchViaCmd(repoRoot)
	}
	s := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
		return ref
	}
	if len(s) >= 7 {
		return s[:7]
	}
	return gitBranchViaCmd(repoRoot)
}

func gitBranchViaCmd(repoRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitStatusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitDirtyCounts(root string, g *gitInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), gitStatusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		x, y := line[0], line[1]
		switch {
		case x == '?' && y == '?':
			g.Untracked++
		default:
			if x != ' ' && x != '?' {
				g.Staged++
			}
			if y != ' ' && y != '?' {
				g.Modified++
			}
		}
	}
}

func (m *Module) terminalSnapshot() string {
	var backend string
	switch {
	case m.useGoShell:
		backend = "built-in Go shell (bash-compatible)"
		if goshell.HasBusybox() {
			backend += " + embedded busybox coreutils"
		}
	case m.path != "":
		backend = m.kind + " at " + m.path
	default:
		backend = m.kind
	}
	parts := []string{
		"OS " + goruntime.GOOS + "/" + goruntime.GOARCH,
		"shell: " + backend,
	}
	if tools := detectTools(); tools != "" {
		parts = append(parts, "available: "+tools)
	}
	return strings.Join(parts, "; ")
}

func detectTools() string {
	var found []string
	for _, t := range []string{"git", "node", "npm", "python", "python3", "go", "docker", "make", "cargo", "rustc", "java", "rg"} {
		if _, err := exec.LookPath(t); err == nil {
			found = append(found, t)
		}
	}
	return strings.Join(found, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
