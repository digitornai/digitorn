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
			if _, skip := skipDirs[d.Name()]; skip {
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
		for i, ln := range splitLines(string(data)) {
			if strings.TrimSpace(ln) == "" || !matchesOutline(ln) {
				continue
			}
			sig := strings.TrimSpace(ln)
			if len(sig) > 200 {
				sig = sig[:200] + " …"
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
	return repomap.Graph{Syms: syms}
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
