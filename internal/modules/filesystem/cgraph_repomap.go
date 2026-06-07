//go:build treesitter

package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
)

const repoMapMaxBytes = 1 << 20

func init() {
	repomap.Register(func(root string) repomap.Graph {
		return extractRepoGraph(root, repoMapMaxBytes)
	})
}

func sigOf(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	return strings.TrimRight(strings.TrimSpace(body), " {")
}

func sigWithKind(kind, body string) string {
	sig := sigOf(body)
	switch {
	case sig == "",
		strings.HasPrefix(sig, "func"),
		strings.HasPrefix(sig, "def"),
		strings.HasPrefix(sig, "class"),
		strings.HasPrefix(sig, "type"),
		strings.HasPrefix(sig, "interface"):
		return sig
	default:
		return strings.TrimSpace(kind + " " + sig)
	}
}

func extractRepoGraph(root string, maxBytes int64) repomap.Graph {
	g := repomap.Graph{Calls: map[string][]string{}}

	var paths []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || sindexIgnoredDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := langForExt(filepath.Ext(path)); !ok {
			return nil
		}
		if info, e := d.Info(); e != nil || (maxBytes > 0 && info.Size() > maxBytes) {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if len(paths) == 0 {
		return g
	}

	type result struct {
		rel string
		fp  fileParse
	}
	workers := runtime.NumCPU()
	if workers > len(paths) {
		workers = len(paths)
	}
	jobs := make(chan string, len(paths))
	for _, p := range paths {
		jobs <- p
	}
	close(jobs)
	results := make(chan result, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				b, e := os.ReadFile(path)
				if e != nil || !utf8.Valid(b) {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				rel = filepath.ToSlash(rel)
				if fp, ok := parseFile(rel, b); ok {
					results <- result{rel: rel, fp: fp}
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	for r := range results {
		for _, s := range r.fp.syms {
			key := r.rel + "#" + s.Name + "#" + strconv.Itoa(s.Start)
			g.Syms = append(g.Syms, repomap.Sym{
				Key: key, Name: s.Name, Kind: s.Kind, File: r.rel,
				Sig: sigWithKind(s.Kind, s.Body), Line: s.Start,
			})
			if len(s.Calls) > 0 {
				g.Calls[key] = s.Calls
			}
		}
	}
	return g
}
