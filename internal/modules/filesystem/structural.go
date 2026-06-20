//go:build treesitter

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbathepaul/digitorn/internal/codeast"
)

// astSearch walks the workspace, parses every AST-supported file with
// treesitter, and returns symbols whose lowercased body contains ALL
// space-separated tokens in pattern. Runs in a recover-guarded goroutine
// with a hard 500ms budget — never blocks the caller.
func (m *Module) astSearch(ctx context.Context, root, pattern string, maxResults int) []astHit {
	tokens := strings.Fields(strings.ToLower(pattern))
	if len(tokens) == 0 {
		return nil
	}
	if maxResults <= 0 {
		maxResults = 50
	}

	gi := loadGitignore(root)
	var hits []astHit

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if isSkipped(name, path) {
				return filepath.SkipDir
			}
			if rel, ok := relUnder(root, path); ok && gi.ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !codeast.Supported(filepath.Ext(path)) {
			return nil
		}
		rel, ok := relUnder(root, path)
		if !ok {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if gi.ignored(relSlash, false) {
			return nil
		}

		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fp, ok := codeast.ParseFile(relSlash, b)
		if !ok {
			return nil
		}

		for _, s := range fp.Syms {
			combined := strings.ToLower(s.Body)
			match := true
			for _, tok := range tokens {
				if !strings.Contains(combined, tok) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			snip := s.Body
			if len(snip) > 400 {
				snip = snip[:400] + "…"
			}
			hits = append(hits, astHit{
				File:    relSlash,
				Line:    s.Start,
				Symbol:  s.Name,
				Kind:    s.Kind,
				Sig:     sigWithKind(s.Kind, s.Body),
				Snippet: snip,
			})
			if len(hits) >= maxResults {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return hits
}
