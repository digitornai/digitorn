package filesystem

import (
	"os"
	"path/filepath"
	"strings"
)

// gitignore.go : a focused .gitignore matcher for glob/grep DISCOVERY (read by
// path is never filtered — only enumeration is). It reads the workspace-root
// .gitignore and applies git's pattern syntax against workspace-relative slash
// paths : blank lines, comments (#), negation (!), anchored (/prefix or a
// mid-pattern slash), directory-only (trailing /), and *, ?, **, [..] globs.
//
// The filter is applied at ENUMERATION time (not at index-build time), so editing
// .gitignore takes effect immediately with no trigram-index rebuild. Nested
// per-directory .gitignore files are not yet honoured (root only) — the common
// case for a repository. Always combined with the hardcoded skipDirs.

type ignoreRule struct {
	negate  bool
	dirOnly bool
	pattern string // glob matched via matchGlob against the rel slash path
}

type ignoreRules struct {
	rules []ignoreRule
}

// loadGitignore reads root/.gitignore and compiles it. Returns nil when there is
// no .gitignore (callers treat nil as "ignore nothing").
func loadGitignore(root string) *ignoreRules {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	return parseGitignore(string(b))
}

func parseGitignore(s string) *ignoreRules {
	g := &ignoreRules{}
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		if strings.Contains(line, "/") {
			anchored = true // a mid-pattern slash anchors to the gitignore root
		}
		if line == "" {
			continue
		}
		if anchored {
			r.pattern = line
		} else {
			r.pattern = "**/" + line // match the basename at any depth
		}
		g.rules = append(g.rules, r)
	}
	if len(g.rules) == 0 {
		return nil
	}
	return g
}

// ignored reports whether the workspace-relative slash path rel is excluded.
// Last matching rule wins, so a later "!negation" can re-include a path.
func (g *ignoreRules) ignored(rel string, isDir bool) bool {
	if g == nil || rel == "" || rel == "." {
		return false
	}
	matched := false
	for _, r := range g.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if matchGlob(r.pattern, rel) {
			matched = !r.negate
		}
	}
	return matched
}

// relUnder returns the slash-form path of abs relative to base (lexical — no
// symlink resolution, which is correct for ignore matching), and ok=false when
// abs is not under base.
func relUnder(base, abs string) (string, bool) {
	if base == "" {
		return "", false
	}
	r, err := filepath.Rel(base, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(r), true
}
