package gitrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	udiff "github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

type noGitlinkStorer struct{ storage.Storer }

const (
	metaDir   = ".digitorn"
	gitSubdir = "git"
)

type Repo struct {
	mu      sync.Mutex
	workdir string
	gitDir  string
	repo    *git.Repository
	wt      *git.Worktree
	origin map[string]plumbing.Hash
}

const originFile = "digitorn-origin.json"

func gitDirOf(workdir string) string { return filepath.Join(workdir, metaDir, gitSubdir) }

func workdirHasContent(workdir string) bool {
	entries, err := os.ReadDir(workdir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if n := e.Name(); n != metaDir && n != ".git" {
			return true
		}
	}
	return false
}

func OpenIfNeeded(workdir string) (*Repo, error) {
	if !headExists(gitDirOf(workdir)) && !workdirHasContent(workdir) {
		return nil, nil
	}
	return Open(workdir)
}

func Open(workdir string) (*Repo, error) {
	gd := gitDirOf(workdir)
	storer := noGitlinkStorer{filesystem.NewStorage(osfs.New(gd), cache.NewObjectLRU(8*1024*1024))}
	wtfs := newPruningFS(osfs.New(workdir), workdir)

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
	wt.Excludes = append(wt.Excludes,
		gitignore.ParsePattern(metaDir+"/", nil),
		gitignore.ParsePattern(".git/", nil),
	)
	ensureExclude(gd)

	r := &Repo{workdir: workdir, gitDir: gd, repo: repo, wt: wt, origin: map[string]plumbing.Hash{}}
	r.loadOrigin()
	return r, nil
}

func (r *Repo) originFilePath() string { return filepath.Join(r.gitDir, originFile) }

func (r *Repo) loadOrigin() {
	b, err := os.ReadFile(r.originFilePath())
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for p, h := range m {
		r.origin[p] = plumbing.NewHash(h)
	}
}

func (r *Repo) saveOriginLocked() {
	m := make(map[string]string, len(r.origin))
	for p, h := range r.origin {
		m[p] = h.String()
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := r.originFilePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, r.originFilePath())
}

func (r *Repo) writeBlobLocked(content string) (plumbing.Hash, error) {
	obj := r.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write([]byte(content)); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.repo.Storer.SetEncodedObject(obj)
}

func (r *Repo) blobContentLocked(h plumbing.Hash) (string, error) {
	blob, err := r.repo.BlobObject(h)
	if err != nil {
		return "", err
	}
	rd, err := blob.Reader()
	if err != nil {
		return "", err
	}
	defer rd.Close()
	b, err := io.ReadAll(rd)
	return string(b), err
}

func (r *Repo) captureOriginLocked(rel string) {
	if _, ok := r.origin[rel]; ok {
		return
	}
	if _, atHead, err := r.headBlobExists(rel); err != nil || atHead {
		return
	}
	content, err := os.ReadFile(filepath.Join(r.workdir, filepath.FromSlash(rel)))
	if err != nil {
		return
	}
	h, err := r.writeBlobLocked(string(content))
	if err != nil {
		return
	}
	r.origin[rel] = h
	r.saveOriginLocked()
}

func (r *Repo) diffBaselineLocked(rel, newContent string) (string, error) {
	head, atHead, err := r.headBlobExists(rel)
	if err != nil {
		return "", err
	}
	if atHead {
		return head, nil
	}
	r.captureOriginLocked(rel)
	h, ok := r.origin[rel]
	if !ok {
		return "", nil
	}
	origin, err := r.blobContentLocked(h)
	if err != nil || newContent == origin {
		return "", nil // unreadable, or untouched since first seen → fresh add
	}
	return origin, nil
}

// pruneCommittedOriginLocked drops origin entries for files now at HEAD (just
// committed): their baseline becomes HEAD, so the snapshot is dead. Keeps the
// map bounded by the not-yet-committed set.
func (r *Repo) pruneCommittedOriginLocked() {
	changed := false
	for rel := range r.origin {
		if _, atHead, err := r.headBlobExists(rel); err == nil && atHead {
			delete(r.origin, rel)
			changed = true
		}
	}
	if changed {
		r.saveOriginLocked()
	}
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
	if err := r.stageAllOnePassLocked(); err != nil {
		return false, err
	}
	_, err := r.commit("baseline: workspace start")
	return true, err
}

// stageAllOnePassLocked stages every pending (non-meta) file by writing the index
// in ONE pass. go-git's AddWithOptions{All:true} rewrites the whole index on each
// file (O(n²)) — minutes on a large tree; here we hash each changed file once and
// SetIndex once (O(n)). The file SET and statuses come from go-git's own Status,
// so .gitignore + the excludes are honoured exactly as AddWithOptions would — the
// resulting tree is identical, just built without the quadratic blow-up.
func (r *Repo) stageAllOnePassLocked() error {
	st, err := r.wt.Status()
	if err != nil {
		return err
	}
	idx, err := r.repo.Storer.Index()
	if err != nil {
		idx = &index.Index{Version: 2}
	}
	if idx.Version == 0 {
		idx.Version = 2
	}
	for p, s := range st {
		if skipPath(p) {
			continue
		}
		rel := filepath.ToSlash(p)
		// A file gone from the worktree (and not still in the index as untracked)
		// is a deletion: drop its entry so the commit records the removal.
		if s.Worktree == git.Deleted {
			_, _ = idx.Remove(rel)
			continue
		}
		ent, err := r.indexEntryForLocked(rel)
		if err != nil {
			return err
		}
		if ent == nil {
			continue
		}
		upsertIndexEntry(idx, ent)
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Name < idx.Entries[j].Name })
	return r.repo.Storer.SetIndex(idx)
}

// indexEntryForLocked builds the index entry for one worktree path: it hashes the
// content into a blob (the link target for a symlink) and stamps the file's mode
// + mtime + size — the same fields go-git's Add records, so the staged tree and
// the index stat data match. Returns nil for a path that vanished mid-walk.
func (r *Repo) indexEntryForLocked(rel string) (*index.Entry, error) {
	abs := filepath.Join(r.workdir, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, nil
	}
	mode, err := filemode.NewFromOSFileMode(info.Mode())
	if err != nil {
		return nil, err
	}
	var content string
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, err
		}
		content = target
	} else {
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		content = string(b)
	}
	h, err := r.writeBlobLocked(content)
	if err != nil {
		return nil, err
	}
	return &index.Entry{
		Name:       rel,
		Hash:       h,
		Mode:       mode,
		Size:       uint32(len(content)),
		ModifiedAt: info.ModTime(),
	}, nil
}

// upsertIndexEntry replaces an existing same-name entry in place, else appends.
func upsertIndexEntry(idx *index.Index, ent *index.Entry) {
	for i := range idx.Entries {
		if idx.Entries[i].Name == ent.Name {
			idx.Entries[i] = ent
			return
		}
	}
	idx.Entries = append(idx.Entries, ent)
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
	// Staged is true when the change is fully in the index with no unstaged
	// remainder — i.e. the user APPROVED it. A later Commit includes only these.
	Staged bool `json:"staged"`
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
		rel := filepath.ToSlash(p)
		// Snapshot the diff baseline on first sight (runs on every debounced
		// poke), so an agent-created file's later edits diff against its first
		// version even if the client fetches the diff late.
		r.captureOriginLocked(rel)
		// Approved = wholly staged (in the index, nothing left in the worktree).
		staged := s.Staging != git.Unmodified && s.Worktree == git.Unmodified
		out = append(out, Change{Path: rel, Status: statusName(code), Staged: staged})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stage adds the given paths to the index — the "approve" action. A later Commit
// then includes ONLY the approved (staged) set. Paths are workdir-relative.
func (r *Repo) Stage(paths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		if err := r.wt.AddWithOptions(&git.AddOptions{Path: filepath.ToSlash(p)}); err != nil {
			return err
		}
	}
	return nil
}

// StageAll stages every pending change (added / modified / deleted) — the
// "approve all" action. The .digitorn metadata and any user .git stay excluded
// via wt.Excludes, exactly as at baseline.
func (r *Repo) StageAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stageAllOnePassLocked()
}

// Restore discards the agent's pending change to each path — the "reject"
// action. A modified/deleted file is rewritten from the baseline (HEAD); a
// newly-added file (absent at HEAD) is removed from disk. The index entry is
// reset so the path drops out of the pending set entirely. Paths are
// workdir-relative; the caller has already confined them to the workdir.
func (r *Repo) Restore(paths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	originDirty := false
	for _, p := range paths {
		rel := filepath.ToSlash(p)
		abs := filepath.Join(r.workdir, filepath.FromSlash(rel))
		if _, ok := r.origin[rel]; ok {
			delete(r.origin, rel) // rejected: its first-seen baseline no longer applies
			originDirty = true
		}
		content, atHead, err := r.headBlobExists(rel)
		if err != nil {
			return err
		}
		if atHead {
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
				return err
			}
			// Re-stage so index == worktree == HEAD → the file is unmodified again.
			if err := r.wt.AddWithOptions(&git.AddOptions{Path: rel}); err != nil {
				return err
			}
		} else {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := r.removeFromIndex(rel); err != nil {
				return err
			}
		}
	}
	if originDirty {
		r.saveOriginLocked()
	}
	return nil
}

// removeFromIndex drops a staged "added" entry so a rejected new file stops
// showing. A no-op when the path was never staged.
func (r *Repo) removeFromIndex(rel string) error {
	idx, err := r.repo.Storer.Index()
	if err != nil {
		return err
	}
	if _, err := idx.Remove(rel); err != nil {
		if errors.Is(err, index.ErrEntryNotFound) {
			return nil
		}
		return err
	}
	return r.repo.Storer.SetIndex(idx)
}

// headBlobExists returns the content of path at HEAD and whether it exists there.
func (r *Repo) headBlobExists(path string) (content string, exists bool, err error) {
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	commit, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false, err
	}
	f, err := commit.File(filepath.ToSlash(path))
	if errors.Is(err, object.ErrFileNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	c, cerr := f.Contents()
	return c, true, cerr
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
	unified, _, _, insertions, deletions, err = r.fileDiffLocked(path)
	return unified, insertions, deletions, err
}

// fileDiffLocked is the body of FileDiff, the caller holding r.mu. It also
// returns the baseline (HEAD) and current (worktree) content so the hunk
// approve/reject ops can apply a SUBSET of the very same diff the client saw —
// guaranteeing the per-hunk hashes match byte-for-byte.
func (r *Repo) fileDiffLocked(path string) (unified, oldContent, newContent string, insertions, deletions int, err error) {
	if b, rerr := os.ReadFile(filepath.Join(r.workdir, filepath.FromSlash(path))); rerr == nil {
		newContent = string(b)
	}
	oldContent, err = r.diffBaselineLocked(filepath.ToSlash(path), newContent)
	if err != nil {
		return "", "", "", 0, 0, err
	}
	if isBinary(oldContent) || isBinary(newContent) {
		return "Binary files differ\n", oldContent, newContent, 0, 0, nil
	}
	diffs := udiff.Do(oldContent, newContent)
	insertions, deletions = countLines(diffs)
	unified, err = encodeUnified(filepath.ToSlash(path), oldContent, newContent, diffs)
	return unified, oldContent, newContent, insertions, deletions, err
}

// Revision is one COMMITTED version of a file — what the "Approval history" tab
// shows. The JSON tags match the web FileHistoryPanel's expected shape.
type Revision struct {
	Revision   int    `json:"revision"`         // 1-based, oldest first
	Message    string `json:"message"`          // approval/commit message (the revision label)
	ApprovedAt int64  `json:"approved_at"`      // commit time (unix seconds)
	ApprovedBy string `json:"approved_by"`      // "user" | "auto"
	InsDelta   int    `json:"tokens_delta_ins"` // lines added vs the previous revision
	DelDelta   int    `json:"tokens_delta_del"` // lines removed vs the previous revision
	Bytes      int    `json:"bytes"`            // file size at this revision
}

// History returns the file's committed revisions (one per shadow-repo commit that
// changed it), oldest first, with the line delta vs the previous revision and the
// size at each. Empty before the first commit — a file becomes a revision only
// when it is approved + committed. Pure read: never mutates the repo.
func (r *Repo) History(path string) ([]Revision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rel := filepath.ToSlash(path)
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return []Revision{}, nil
	}
	if err != nil {
		return nil, err
	}
	iter, err := r.repo.Log(&git.LogOptions{From: ref.Hash(), FileName: &rel, Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, err
	}
	var commits []*object.Commit // newest first
	if err := iter.ForEach(func(c *object.Commit) error { commits = append(commits, c); return nil }); err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(commits))
	prev := "" // content at the previous (older) revision
	for i := len(commits) - 1; i >= 0; i-- {
		content := fileAtCommit(commits[i], rel)
		ins, del := 0, 0
		if !isBinary(prev) && !isBinary(content) {
			ins, del = countLines(udiff.Do(prev, content))
		}
		out = append(out, Revision{
			Revision:   len(out) + 1,
			Message:    strings.TrimSpace(commits[i].Message),
			ApprovedAt: commits[i].Committer.When.Unix(),
			ApprovedBy: "user",
			InsDelta:   ins,
			DelDelta:   del,
			Bytes:      len(content),
		})
		prev = content
	}
	return out, nil
}

// RestoreRevision rewrites the working-tree file with its content at the given
// 1-based revision (oldest-first, matching History). It does NOT touch the
// shadow repo's history — the reverted content lands on disk as a fresh PENDING
// change the user reviews then approves (a new revision on top) or rejects. Pure
// from the repo's standpoint: only the worktree file is written.
func (r *Repo) RestoreRevision(path string, revision int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if revision < 1 {
		return fmt.Errorf("invalid revision %d", revision)
	}
	rel := filepath.ToSlash(path)
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("no history for %s", rel)
	}
	if err != nil {
		return err
	}
	iter, err := r.repo.Log(&git.LogOptions{From: ref.Hash(), FileName: &rel, Order: git.LogOrderCommitterTime})
	if err != nil {
		return err
	}
	var commits []*object.Commit // newest first
	if err := iter.ForEach(func(c *object.Commit) error { commits = append(commits, c); return nil }); err != nil {
		return err
	}
	if revision > len(commits) {
		return fmt.Errorf("revision %d out of range (have %d)", revision, len(commits))
	}
	// revision is 1-based oldest-first; commits is newest-first.
	content := fileAtCommit(commits[len(commits)-revision], rel)
	abs := filepath.Join(r.workdir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// Commit is one entry of the workspace's GLOBAL history (every approval =
// a shadow-repo commit), with the files that commit changed.
type Commit struct {
	Sha        string   `json:"sha"`
	Message    string   `json:"message"`
	ApprovedAt int64    `json:"approved_at"`
	ApprovedBy string   `json:"approved_by"`
	Files      []string `json:"files"`
}

// Log returns the whole shadow-repo history, newest first, each commit carrying
// the files it changed. Empty before the first commit.
func (r *Repo) Log() ([]Commit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return []Commit{}, nil
	}
	if err != nil {
		return nil, err
	}
	iter, err := r.repo.Log(&git.LogOptions{From: ref.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, err
	}
	out := []Commit{}
	if err := iter.ForEach(func(c *object.Commit) error {
		files, ferr := commitFiles(c)
		if ferr != nil {
			return ferr
		}
		out = append(out, Commit{
			Sha:        c.Hash.String(),
			Message:    strings.TrimSpace(c.Message),
			ApprovedAt: c.Committer.When.Unix(),
			ApprovedBy: "user",
			Files:      files,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// HeadSHA returns the current HEAD commit sha, or "" before the first commit.
// Used to record what has been pushed to a remote and to count unpushed commits.
func (r *Repo) HeadSHA() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

// commitFiles lists the non-meta paths a commit changed vs its first parent (the
// whole tree for the root/baseline commit), sorted.
func commitFiles(c *object.Commit) ([]string, error) {
	cur, err := c.Tree()
	if err != nil {
		return nil, err
	}
	files := []string{}
	if c.NumParents() == 0 {
		if err := cur.Files().ForEach(func(f *object.File) error {
			if !skipPath(f.Name) {
				files = append(files, f.Name)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		sort.Strings(files)
		return files, nil
	}
	parent, err := c.Parent(0)
	if err != nil {
		return nil, err
	}
	ptree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := ptree.Diff(cur)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, ch := range changes {
		name := ch.To.Name
		if name == "" {
			name = ch.From.Name
		}
		if name == "" || skipPath(name) || seen[name] {
			continue
		}
		seen[name] = true
		files = append(files, name)
	}
	sort.Strings(files)
	return files, nil
}

// RestoreCommit writes the given files back to their content AT the commit
// `sha` (every file the commit changed when `paths` is empty) — a "go back to
// this point" restore. Like RestoreRevision it only touches the working tree:
// the result is a PENDING change the user reviews then approves or rejects.
func (r *Repo) RestoreCommit(sha string, paths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return fmt.Errorf("commit %s: %w", sha, err)
	}
	if len(paths) == 0 {
		if paths, err = commitFiles(c); err != nil {
			return err
		}
	}
	for _, p := range paths {
		rel := filepath.ToSlash(p)
		if skipPath(rel) {
			continue
		}
		content := fileAtCommit(c, rel)
		abs := filepath.Join(r.workdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fileAtCommit returns the file's content at a commit, or "" if absent there.
func fileAtCommit(c *object.Commit, path string) string {
	f, err := c.File(path)
	if err != nil {
		return ""
	}
	s, err := f.Contents()
	if err != nil {
		return ""
	}
	return s
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

// ErrNothingStaged is returned by Commit when no change has been approved
// (staged) yet — so we never advance the baseline with an empty commit.
var ErrNothingStaged = errors.New("nothing approved to commit")

// Commit commits the APPROVED (staged) set, advancing HEAD so those files stop
// showing as pending. Any paths passed are staged first (commit-with-selection);
// unstaged pending files are deliberately left untouched. Returns
// ErrNothingStaged when there is nothing to commit.
func (r *Repo) Commit(message string, paths []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		if err := r.wt.AddWithOptions(&git.AddOptions{Path: filepath.ToSlash(p)}); err != nil {
			return "", err
		}
	}
	staged, err := r.hasStagedLocked()
	if err != nil {
		return "", err
	}
	if !staged {
		return "", ErrNothingStaged
	}
	if message == "" {
		message = "validate workspace changes"
	}
	h, err := r.commit(message)
	if err != nil {
		return "", err
	}
	r.pruneCommittedOriginLocked()
	return h.String(), nil
}

// hasStagedLocked reports whether any non-meta path is staged. Caller holds r.mu.
func (r *Repo) hasStagedLocked() (bool, error) {
	st, err := r.wt.Status()
	if err != nil {
		return false, err
	}
	for p, s := range st {
		if skipPath(p) {
			continue
		}
		// Untracked is NOT staged — only a real index change vs HEAD counts.
		if s.Staging != git.Unmodified && s.Staging != git.Untracked {
			return true, nil
		}
	}
	return false, nil
}

// PushToRemote pushes HEAD to a GitHub repo over HTTPS with the user's OAuth
// token (never persisted in git config).
func (r *Repo) PushToRemote(remoteURL, token, branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if branch == "" {
		branch = "main"
	}
	_ = r.repo.DeleteRemote("github")
	if _, err := r.repo.CreateRemote(&gitconfig.RemoteConfig{
		Name: "github",
		URLs: []string{remoteURL},
	}); err != nil {
		return err
	}
	head, err := r.repo.Head()
	if err != nil {
		return err
	}
	spec := gitconfig.RefSpec(fmt.Sprintf("%s:refs/heads/%s", head.Name(), branch))
	err = r.repo.Push(&git.PushOptions{
		RemoteName: "github",
		RefSpecs:   []gitconfig.RefSpec{spec},
		Auth:       &githttp.BasicAuth{Username: "x-access-token", Password: token},
		Force:      false,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}
