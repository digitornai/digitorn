package gitrepo

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5"
)

var prunedDirs = map[string]bool{
	".git":         true,
	metaDir:        true,
	"node_modules": true,
}

type pruningFS struct {
	billy.Filesystem
	extraPruned map[string]bool
}

func newPruningFS(fs billy.Filesystem, workdir string) pruningFS {
	return pruningFS{Filesystem: fs, extraPruned: loadGitignoreDirs(workdir)}
}

func loadGitignoreDirs(workdir string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(filepath.Join(workdir, ".gitignore"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		name := strings.TrimSuffix(line, "/")
		if strings.ContainsAny(name, "/*?[") {
			continue
		}
		out[name] = true
	}
	return out
}

func (f pruningFS) ReadDir(path string) ([]os.FileInfo, error) {
	ents, err := f.Filesystem.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := ents[:0]
	for _, e := range ents {
		if e.IsDir() && (prunedDirs[e.Name()] || f.extraPruned[e.Name()]) {
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
	return pruningFS{Filesystem: sub, extraPruned: f.extraPruned}, nil
}
