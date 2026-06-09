package gitrepo

import (
	"os"

	"github.com/go-git/go-billy/v5"
)

// prunedDirs are directory basenames go-git's worktree walk must never descend
// into. They are dependency / VCS metadata trees that hold no agent change worth
// tracking and that the shadow repo already excludes anyway — but go-git, unlike
// native git, would still stat every file inside them to apply the ignore rules.
// Hiding them at the filesystem boundary keeps the walk O(real files), not
// O(repo). The set is deliberately tiny: only dirs that are NEVER tracked agent
// changes, so the visible status is identical to walking the whole tree.
var prunedDirs = map[string]bool{
	".git":         true,
	metaDir:        true, // .digitorn (the shadow repo lives here)
	"node_modules": true,
}

// pruningFS wraps a billy.Filesystem and removes prunedDirs from every ReadDir,
// so go-git never recurses into them. Only the worktree-walk surface (ReadDir,
// and Chroot which go-git uses to descend) is overridden; all other operations
// pass straight through. Pure read-side filter: it changes nothing on disk.
type pruningFS struct {
	billy.Filesystem
}

func newPruningFS(fs billy.Filesystem) pruningFS { return pruningFS{Filesystem: fs} }

func (f pruningFS) ReadDir(path string) ([]os.FileInfo, error) {
	ents, err := f.Filesystem.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := ents[:0]
	for _, e := range ents {
		if e.IsDir() && prunedDirs[e.Name()] {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (f pruningFS) Chroot(path string) (billy.Filesystem, error) {
	sub, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return pruningFS{Filesystem: sub}, nil
}
