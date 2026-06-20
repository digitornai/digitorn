//go:build !treesitter

package filesystem

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
)

// Without treesitter there is no call graph, so the injected codebase map used to
// be EMPTY (only the treesitter build registered an extractor). That left an agent
// with no project overview — forcing it to glob+read to orient. This fallback
// registers a regex-outliner-based extractor (the same heuristic `read outline`
// uses) so the codebase map works in EVERY build, including the Linux/prod one
// where the treesitter cgo cross-compile is skipped. No PageRank signal (no call
// edges), but a complete file→definitions skeleton — the bulk of the value.
func init() {
	repomap.Register(fallbackRepoGraph)
}

const fallbackRepoMaxSyms = 4000

func fallbackRepoGraph(root string) repomap.Graph {
	ignore := loadGitignore(root)
	var syms []repomap.Sym
	calls := map[string][]string{}
	fileImports := map[string][]string{} // rel → import paths

	_ = filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil || abs == root || len(syms) >= fallbackRepoMaxSyms {
			if d != nil && d.IsDir() && len(syms) >= fallbackRepoMaxSyms {
				return filepath.SkipDir
			}
			return nil
		}
		rel, ok := relInside(root, abs)
		if !ok || rel == "." {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if isSkipped(d.Name(), abs) {
				return filepath.SkipDir
			}
			if ignore != nil {
				if r, ok := relUnder(root, abs); ok && ignore.ignored(r, true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if ignore != nil {
			if r, ok := relUnder(root, abs); ok && ignore.ignored(r, false) {
				return nil
			}
		}
		data, _, e := readCapped(abs, dirOutlineFileCap)
		if e != nil || len(data) == 0 {
			return nil
		}
		head := data
		if len(head) > 8192 {
			head = head[:8192]
		}
		if detectKind(head).kind != "text" {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		// Extract imports for dependency graph
		if imports := extractGoImports(data); len(imports) > 0 {
			fileImports[relSlash] = imports
		}

		lines := splitLines(string(data))
		for i, ln := range lines {
			if strings.TrimSpace(ln) == "" || !matchesOutline(ln) {
				continue
			}
			sig := strings.TrimSpace(ln)
			if len(sig) > 200 {
				sig = sig[:200] + " …"
			}
			// Prepend docstring if available
			if doc := extractDocstring(lines, i+1); doc != "" {
				sig = "// " + doc + "\n  " + sig
			}
			syms = append(syms, repomap.Sym{
				Key:  relSlash + ":" + strconv.Itoa(i+1),
				Name: defName(sig),
				File: relSlash,
				Sig:  sig,
				Line: i + 1,
			})
			if len(syms) >= fallbackRepoMaxSyms {
				break
			}
		}
		return nil
	})

	// Build import-based dependency edges: for each file, create synthetic
	// call edges from its symbols to the symbols of imported files. This gives
	// PageRank a real signal — files that are imported by many others rank higher.
	modulePrefix := detectModulePrefix(root)
	for file, imports := range fileImports {
		for _, imp := range imports {
			target := importPathToFile(imp, modulePrefix, root)
			if target == "" || target == file {
				continue
			}
			for _, s := range syms {
				if s.File == file {
					for _, t := range syms {
						if t.File == target {
							calls[s.Key] = append(calls[s.Key], t.Name)
						}
					}
				}
			}
		}
	}

	return repomap.Graph{Syms: syms, Calls: calls}
}

// defName best-effort extracts the declared symbol name from a definition line —
// the token after the keyword, skipping a Go method receiver group "(m *T)".
func defName(sig string) string {
	fields := strings.Fields(sig)
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "func", "type", "class", "def", "const", "var", "interface", "struct", "enum", "trait", "impl", "module", "function":
			j := i + 1
			if j < len(fields) && strings.HasPrefix(fields[j], "(") { // skip "(recv *Type)"
				for j < len(fields) && !strings.Contains(fields[j], ")") {
					j++
				}
				j++
			}
			if j < len(fields) {
				name := strings.TrimLeft(fields[j], "*(")
				if k := strings.IndexAny(name, "(<{[:"); k > 0 {
					name = name[:k]
				}
				if name != "" {
					return name
				}
			}
		}
	}
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return sig
}
// extractGoImports extracts import paths from a Go source file.
func extractGoImports(data []byte) []string {
	var imports []string
	inBlock := false
	for _, ln := range splitLines(string(data)) {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "import (" {
			inBlock = true
			continue
		}
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				continue
			}
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			// strip inline comments and quotes
			if idx := strings.Index(trimmed, "//"); idx > 0 {
				trimmed = strings.TrimSpace(trimmed[:idx])
			}
			trimmed = strings.Trim(trimmed, "\"")
			if trimmed != "" {
				imports = append(imports, trimmed)
			}
			continue
		}
		// single-line import: import "path"
		if strings.HasPrefix(trimmed, "import ") {
			p := strings.TrimPrefix(trimmed, "import ")
			p = strings.Trim(p, "\"")
			if p != "" {
				imports = append(imports, p)
			}
		}
	}
	return imports
}

// extractDocstring returns the last contiguous comment block immediately above
// the given line number (1-based). Returns "" when no comment is found.
func extractDocstring(lines []string, defLine int) string {
	if defLine < 1 || defLine > len(lines) {
		return ""
	}
	// walk backwards from the line just above the definition
	var docLines []string
	for i := defLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			break
		}
		if strings.HasPrefix(trimmed, "//") {
			comment := strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
			docLines = append([]string{comment}, docLines...)
		} else {
			break
		}
	}
	if len(docLines) == 0 {
		return ""
	}
	doc := strings.Join(docLines, " ")
	if len(doc) > 120 {
		doc = doc[:120] + "…"
	}
	return doc
}

// importPathToFile converts a Go import path like
// "github.com/mbathepaul/digitorn/internal/foo" to a relative file path by
// stripping the module prefix. Returns "" for standard library or unresolvable.
func importPathToFile(impPath, modulePrefix, root string) string {
	if !strings.HasPrefix(impPath, modulePrefix) {
		return "" // external / stdlib
	}
	rel := strings.TrimPrefix(impPath, modulePrefix)
	rel = strings.TrimPrefix(rel, "/")
	// Try common file patterns: package.go, then package/package.go
	candidates := []string{
		rel + ".go",
		rel + "/index.go",
		rel + "/module.go",
	}
	for _, c := range candidates {
		abs := filepath.Join(root, filepath.FromSlash(c))
		if _, err := os.Stat(abs); err == nil {
			return filepath.ToSlash(c)
		}
	}
	// Return the directory as a fallback — at least the package grouping works
	return rel
}

// detectModulePrefix reads go.mod to find the module path (e.g.
// "github.com/mbathepaul/digitorn") so import paths can be mapped to files.
func detectModulePrefix(root string) string {
	data, _, err := readCapped(filepath.Join(root, "go.mod"), 4096)
	if err != nil || len(data) == 0 {
		return ""
	}
	for _, ln := range splitLines(string(data)) {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "module "))
		}
	}
	return ""
}
