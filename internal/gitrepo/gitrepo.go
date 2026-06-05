// Package gitrepo is a per-workspace SHADOW git repository used to track ONLY
// the agent's file changes, without ever touching a user's own .git that may
// already live in the workdir.
//
// The git data lives under <workdir>/.digitorn/git while the worktree is the
// <workdir> itself (go-git lets the object store and the worktree sit in
// different places). The first commit (HEAD) is the workspace's starting state,
// so every later modification is a clean diff against that baseline — no
// hand-maintained per-file baselines that drift (the bug in the old system).
//
// It is a pure package: it runs inside the workspace worker, never on the
// daemon hot path.
package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	udiff "github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// noGitlinkStorer hides the storer's Filesystem() method so go-git does NOT
// write a `.git` gitlink file into the worktree. That gitlink would collide
// with a user's pre-existing .git (and pollute even a fresh workdir); go-git
// uses the worktree we pass it explicitly, so the link is unnecessary for our
// in-process use. The shadow git data lives only under .digitorn/git.
type noGitlinkStorer struct{ storage.Storer }

const (
	metaDir   = ".digitorn"
	gitSubdir = "git"
)

// Repo is the shadow repo for one workspace (session workdir). All exported
// operations are serialised by mu so a Repo is safe for concurrent use (go-git
// worktree ops are not re-entrant); the worker still issues one refresh per
// session at a time, so this only guards against misuse.
type Repo struct {
	mu      sync.Mutex
	workdir string
	gitDir  string
	repo    *git.Repository
	wt      *git.Worktree
}

func gitDirOf(workdir string) string { return filepath.Join(workdir, metaDir, gitSubdir) }

// Open opens — initialising on first use — the shadow repo for workdir.
func Open(workdir string) (*Repo, error) {
	gd := gitDirOf(workdir)
	storer := noGitlinkStorer{filesystem.NewStorage(osfs.New(gd), cache.NewObjectLRUDefault())}
	wtfs := osfs.New(workdir)

	var (
		repo *git.Repository
		err  error
	)
	if headExists(gd) {
		repo, err = git.Open(storer, wtfs)
	} else {
		if err = os.MkdirAll(gd, 0o755); err != nil {
			return nil, err
		}
		repo, err = git.Init(storer, wtfs)
	}
	if err != nil {
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	// Never track our own metadata folder, nor a user's pre-existing .git.
	// In-memory excludes drive go-git; the on-disk info/exclude keeps the real
	// git CLI (and any reopen) consistent with the same rule.
	wt.Excludes = append(wt.Excludes,
		gitignore.ParsePattern(metaDir+"/", nil),
		gitignore.ParsePattern(".git/", nil),
	)
	ensureExclude(gd)

	return &Repo{workdir: workdir, gitDir: gd, repo: repo, wt: wt}, nil
}

// ensureExclude persists the exclude rules to <gitDir>/info/exclude so the real
// git CLI honours them too (used for cross-validation and any external access).
func ensureExclude(gitDir string) {
	info := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return
	}
	p := filepath.Join(info, "exclude")
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.WriteFile(p, []byte(".git/\n"+metaDir+"/\n"), 0o644)
}

func headExists(gitDir string) bool {
	_, err := os.Stat(filepath.Join(gitDir, "HEAD"))
	return err == nil
}

// EnsureBaseline makes the first commit (HEAD) if the repo has none yet, so
// every later change is a diff against the workspace's starting state. Returns
// true when it created the baseline.
func (r *Repo) EnsureBaseline() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.repo.Head(); err == nil {
		return false, nil
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, err
	}
	if err := r.wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return false, err
	}
	_, err := r.commit("baseline: workspace start")
	return true, err
}

func (r *Repo) commit(msg string) (plumbing.Hash, error) {
	return r.wt.Commit(msg, &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            signature(),
	})
}

func signature() *object.Signature {
	return &object.Signature{Name: "digitorn", Email: "agent@digitorn.local", When: time.Now()}
}

// Change is one file the agent added/modified/deleted since HEAD.
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added | modified | deleted | renamed | copied
}

// Changes lists the agent's pending modifications (working tree vs HEAD),
// excluding the .digitorn metadata and any user .git.
func (r *Repo) Changes() ([]Change, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.wt.Status()
	if err != nil {
		return nil, err
	}
	out := make([]Change, 0, len(st))
	for p, s := range st {
		if skipPath(p) {
			continue
		}
		code := s.Worktree
		if code == git.Unmodified {
			code = s.Staging
		}
		if code == git.Unmodified {
			continue
		}
		out = append(out, Change{Path: filepath.ToSlash(p), Status: statusName(code)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func skipPath(p string) bool {
	p = filepath.ToSlash(p)
	return p == metaDir || strings.HasPrefix(p, metaDir+"/") ||
		p == ".git" || strings.HasPrefix(p, ".git/")
}

func statusName(c git.StatusCode) string {
	switch c {
	case git.Untracked, git.Added:
		return "added"
	case git.Modified:
		return "modified"
	case git.Deleted:
		return "deleted"
	case git.Renamed:
		return "renamed"
	case git.Copied:
		return "copied"
	default:
		return "modified"
	}
}

// FileDiff returns the unified diff of one file (HEAD vs working tree) plus
// per-file insertion / deletion line counts.
func (r *Repo) FileDiff(path string) (unified string, insertions, deletions int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldContent, err := r.headBlob(path)
	if err != nil {
		return "", 0, 0, err
	}
	newContent := ""
	if b, rerr := os.ReadFile(filepath.Join(r.workdir, filepath.FromSlash(path))); rerr == nil {
		newContent = string(b)
	}
	if isBinary(oldContent) || isBinary(newContent) {
		return "Binary files differ\n", 0, 0, nil
	}
	diffs := udiff.Do(oldContent, newContent)
	insertions, deletions = countLines(diffs)
	unified, err = encodeUnified(filepath.ToSlash(path), oldContent, newContent, diffs)
	return unified, insertions, deletions, err
}

// isBinary mirrors git's heuristic: a NUL byte in the first 8000 bytes marks
// the content as binary (so we never emit a garbage line diff for it).
func isBinary(s string) bool {
	n := len(s)
	if n > 8000 {
		n = 8000
	}
	return strings.IndexByte(s[:n], 0) >= 0
}

func countLines(diffs []diffmatchpatch.Diff) (ins, del int) {
	for _, d := range diffs {
		if d.Text == "" {
			continue
		}
		// diffmatchpatch line mode yields whole lines; the last line of a
		// segment may lack a trailing newline (git counts it as a line too).
		n := strings.Count(d.Text, "\n")
		if !strings.HasSuffix(d.Text, "\n") {
			n++
		}
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			ins += n
		case diffmatchpatch.DiffDelete:
			del += n
		}
	}
	return ins, del
}

// headBlob returns the content of path at HEAD, or "" if there is no HEAD
// commit or the file does not exist there (a newly added file).
func (r *Repo) headBlob(path string) (string, error) {
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	commit, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		return "", err
	}
	f, err := commit.File(filepath.ToSlash(path))
	if errors.Is(err, object.ErrFileNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return f.Contents()
}

// Commit stages the given paths (or all pending changes when paths is empty)
// and commits them — advancing HEAD so they stop showing as pending. This is
// the "validate" action triggered from the client.
func (r *Repo) Commit(message string, paths []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(paths) == 0 {
		if err := r.wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			return "", err
		}
	} else {
		for _, p := range paths {
			if err := r.wt.AddWithOptions(&git.AddOptions{Path: filepath.ToSlash(p)}); err != nil {
				return "", err
			}
		}
	}
	if message == "" {
		message = "validate workspace changes"
	}
	h, err := r.commit(message)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}
