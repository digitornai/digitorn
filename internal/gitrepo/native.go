// native.go — ops on a workspace's real git repo (<workdir>/.git) for sessions
// bound to a cloned GitHub repo. Separate from the shadow repo; .digitorn/ is
// excluded from the real repo so shadow data is never pushed upstream.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

var ErrWorkdirNotEmpty = errors.New("gitrepo: workspace is not empty")

var ErrNoNativeRepo = errors.New("gitrepo: workspace has no git repository")

func nativeAuth(token string) *githttp.BasicAuth {
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}
}

func nativeOpen(workdir string) (*git.Repository, *git.Worktree, error) {
	repo, err := git.PlainOpen(workdir)
	if err != nil {
		return nil, nil, ErrNoNativeRepo
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, nil, err
	}
	wt.Excludes = append(wt.Excludes, gitignore.ParsePattern(metaDir+"/", nil))
	return repo, wt, nil
}

// CloneRepo clones remoteURL into an unused workdir (nothing on disk but the
// daemon's .digitorn metadata). Returns the checked-out branch and HEAD sha.
func CloneRepo(ctx context.Context, workdir, remoteURL, branch, token string) (string, string, error) {
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err == nil {
		return "", "", fmt.Errorf("%w: a git repository already exists", ErrWorkdirNotEmpty)
	}
	if headExists(gitDirOf(workdir)) {
		return "", "", fmt.Errorf("%w: the workspace already has tracked history", ErrWorkdirNotEmpty)
	}
	if workdirHasContent(workdir) {
		return "", "", fmt.Errorf("%w: it already contains files", ErrWorkdirNotEmpty)
	}
	opts := &git.CloneOptions{URL: remoteURL}
	if a := nativeAuth(token); a != nil {
		opts.Auth = a
	}
	if branch != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(branch)
		opts.SingleBranch = true
	}
	repo, err := git.PlainCloneContext(ctx, workdir, false, opts)
	if err != nil {
		return "", "", err
	}
	info := filepath.Join(workdir, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(info, "exclude"), []byte(metaDir+"/\n"), 0o644)
	}
	head, err := repo.Head()
	if err != nil {
		return "", "", err
	}
	return head.Name().Short(), head.Hash().String(), nil
}

// InitRepo turns an unused workdir into a fresh native checkout bound to
// remoteURL (a just-created empty GitHub repo): git init on `branch` + origin.
func InitRepo(workdir, remoteURL, branch string) error {
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err == nil {
		return fmt.Errorf("%w: a git repository already exists", ErrWorkdirNotEmpty)
	}
	if headExists(gitDirOf(workdir)) {
		return fmt.Errorf("%w: the workspace already has tracked history", ErrWorkdirNotEmpty)
	}
	if branch == "" {
		branch = "main"
	}
	repo, err := git.PlainInitWithOptions(workdir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	})
	if err != nil {
		return err
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}}); err != nil {
		return err
	}
	info := filepath.Join(workdir, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(info, "exclude"), []byte(metaDir+"/\n"), 0o644)
	}
	return nil
}

// NativeStatus reports pending (uncommitted) paths and the current HEAD sha.
func NativeStatus(workdir string) (uncommitted int, head string, err error) {
	repo, wt, err := nativeOpen(workdir)
	if err != nil {
		return 0, "", err
	}
	st, err := wt.Status()
	if err != nil {
		return 0, "", err
	}
	for _, s := range st {
		if s.Worktree != git.Unmodified || s.Staging != git.Unmodified {
			uncommitted++
		}
	}
	if h, herr := repo.Head(); herr == nil {
		head = h.Hash().String()
	}
	return uncommitted, head, nil
}

// NativeSync stages everything, commits when dirty, and pushes `branch` to origin.
func NativeSync(ctx context.Context, workdir, token, message, authorName, authorEmail, branch string) (string, bool, error) {
	repo, wt, err := nativeOpen(workdir)
	if err != nil {
		return "", false, err
	}
	st, err := wt.Status()
	if err != nil {
		return "", false, err
	}
	committed := false
	if !st.IsClean() {
		if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			return "", false, err
		}
		if authorName == "" {
			authorName = "Digitorn"
		}
		if authorEmail == "" {
			authorEmail = "noreply@digitorn.ai"
		}
		sig := &object.Signature{Name: authorName, Email: authorEmail, When: time.Now()}
		if _, err := wt.Commit(message, &git.CommitOptions{Author: sig}); err != nil {
			return "", false, err
		}
		committed = true
	}
	pushOpts := &git.PushOptions{RemoteName: "origin"}
	if branch != "" {
		spec := config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)
		pushOpts.RefSpecs = []config.RefSpec{spec}
	}
	if a := nativeAuth(token); a != nil {
		pushOpts.Auth = a
	}
	if err := repo.PushContext(ctx, pushOpts); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", committed, err
	}
	head, err := repo.Head()
	if err != nil {
		return "", committed, err
	}
	return head.Hash().String(), committed, nil
}

// NativePull fast-forwards from origin; updated=false when already up to date.
func NativePull(ctx context.Context, workdir, token string) (string, bool, error) {
	repo, wt, err := nativeOpen(workdir)
	if err != nil {
		return "", false, err
	}
	opts := &git.PullOptions{RemoteName: "origin"}
	if a := nativeAuth(token); a != nil {
		opts.Auth = a
	}
	updated := true
	if err := wt.PullContext(ctx, opts); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return "", false, err
		}
		updated = false
	}
	head, err := repo.Head()
	if err != nil {
		return "", updated, err
	}
	return head.Hash().String(), updated, nil
}
