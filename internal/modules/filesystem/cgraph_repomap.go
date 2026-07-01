//go:build treesitter

package filesystem

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/digitornai/digitorn/internal/codeast"
	"github.com/digitornai/digitorn/internal/runtime/context/repomap"
)

const repoMapMaxBytes = 1 << 20

func init() {
	repomap.RegisterIncremental(walkRepo, parseOneFile)
}

// walkRepo returns all treesitter-parseable files under root, applying
// .gitignore and sindexIgnoredDirs. Used by the incremental repomap builder.
func walkRepo(root string) []repomap.WalkEntry {
	gi := loadGitignore(root)
	var out []repomap.WalkEntry
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || sindexIgnoredDirs[d.Name()] {
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
		info, err := d.Info()
		if err != nil || (repoMapMaxBytes > 0 && info.Size() > repoMapMaxBytes) {
			return nil
		}
		out = append(out, repomap.WalkEntry{
			Abs:     path,
			Rel:     relSlash,
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
		return nil
	})
	return out
}

// parseOneFile parses a single source file with treesitter and returns its
// symbols, call edges, and package name. Used by the incremental repomap.
func parseOneFile(rel string, content []byte) (repomap.FileSyms, bool) {
	fp, ok := codeast.ParseFile(rel, content)
	if !ok {
		return repomap.FileSyms{}, false
	}
	syms := make([]repomap.Sym, 0, len(fp.Syms))
	calls := make(map[string][]string, len(fp.Syms))
	pkg := extractPackageName(content)

	for _, s := range fp.Syms {
		key := rel + "#" + s.Name + "#" + strconv.Itoa(s.Start)
		fields := extractFields(s.Kind, s.Body)
		syms = append(syms, repomap.Sym{
			Key:     key,
			Name:    s.Name,
			Kind:    s.Kind,
			File:    rel,
			Sig:     sigWithKind(s.Kind, s.Body),
			Fields:  fields,
			Package: pkg,
			Line:    s.Start,
			EndLine: s.End,
		})
		if len(s.Calls) > 0 {
			calls[key] = s.Calls
		}
	}
	return repomap.FileSyms{Package: pkg, Syms: syms, Calls: calls}, true
}

// sigOf extracts the first line of a symbol body as its signature.
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

// sigWithKind builds the display signature for a symbol.
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

// extractFields returns the first up to 3 non-empty, non-brace field lines
// from a struct or interface body. Returns "" for other kinds.
func extractFields(kind, body string) string {
	lower := strings.ToLower(kind)
	if lower != "struct" && lower != "interface" &&
		!strings.Contains(body, " struct") && !strings.Contains(body, " interface") {
		return ""
	}
	lines := strings.Split(body, "\n")
	var kept []string
	for _, ln := range lines[1:] { // skip first line (the type declaration)
		t := strings.TrimSpace(ln)
		if t == "" || t == "{" || t == "}" {
			continue
		}
		// strip inline comments
		if idx := strings.Index(t, "//"); idx > 0 {
			t = strings.TrimSpace(t[:idx])
		}
		if t == "" {
			continue
		}
		kept = append(kept, t)
		if len(kept) == 3 {
			break
		}
	}
	return strings.Join(kept, "\n")
}

// extractPackageName reads the first `package X` declaration from Go source.
// Returns "" for non-Go or unparseable files.
func extractPackageName(src []byte) string {
	for _, ln := range strings.SplitN(string(src), "\n", 30) {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "//") || t == "" {
			continue
		}
		if strings.HasPrefix(t, "package ") {
			pkg := strings.TrimPrefix(t, "package ")
			if i := strings.IndexAny(pkg, " \t//"); i > 0 {
				pkg = pkg[:i]
			}
			return strings.TrimSpace(pkg)
		}
		break // first non-comment non-blank line is not a package decl
	}
	return ""
}

// extractRepoGraph is kept for backward-compat with the benchmark test.
// It performs a full (non-incremental) build using walkRepo + parseOneFile.
func extractRepoGraph(root string, maxBytes int64) repomap.Graph {
	entries := walkRepo(root)
	g := repomap.Graph{Calls: map[string][]string{}}
	for _, e := range entries {
		if maxBytes > 0 && e.Size > maxBytes {
			continue
		}
		b, err := os.ReadFile(e.Abs)
		if err != nil {
			continue
		}
		fs, ok := parseOneFile(e.Rel, b)
		if !ok {
			continue
		}
		g.Syms = append(g.Syms, fs.Syms...)
		for k, calls := range fs.Calls {
			g.Calls[k] = calls
		}
	}
	return g
}
