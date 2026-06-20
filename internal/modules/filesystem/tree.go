package filesystem

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
)

const (
	treeMaxEntries = 20000 // large codebases can have thousands of files
	treeMaxDepth   = 20   // deep enough for any real nesting

	dirOutlineMaxFiles = 2000
	dirOutlineFileCap  = 512 << 10
)

// ── metadata helpers ────────────────────────────────────────────────────────

// fileMetadata holds per-file enrichment collected in parallel.
type fileMetadata struct {
	lines   int
	pkg     string
	topSyms []string // up to 4 key symbol names (from repomap cache or quickpkg)
}

// collectMetadata reads a batch of files in parallel and returns per-abs-path
// metadata. root is the workspace root for repomap cache lookup (may be "").
func collectMetadata(root string, absPaths []string, relPaths []string) map[string]fileMetadata {
	if len(absPaths) == 0 {
		return nil
	}
	type job struct{ abs, rel string }
	jobs := make(chan job, len(absPaths))
	for i, abs := range absPaths {
		jobs <- job{abs, relPaths[i]}
	}
	close(jobs)

	type res struct {
		abs  string
		meta fileMetadata
	}
	nw := runtime.NumCPU()
	if nw > len(absPaths) {
		nw = len(absPaths)
	}
	outCh := make(chan res, nw*2)
	var wg sync.WaitGroup
	for i := 0; i < nw; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				m := fileMetadata{}
				data, err := os.ReadFile(j.abs)
				if err == nil {
					m.lines = bytes.Count(data, []byte{'\n'}) + 1
					m.pkg = quickPkg(data)
				}
				if root != "" {
					if fs, ok := repomap.LookupFileSyms(root, filepath.ToSlash(j.rel)); ok {
						if fs.Package != "" {
							m.pkg = fs.Package
						}
						seen := map[string]bool{}
						for _, s := range fs.Syms {
							if seen[s.Name] || s.Name == "" {
								continue
							}
							seen[s.Name] = true
							m.topSyms = append(m.topSyms, s.Name)
							if len(m.topSyms) == 4 {
								break
							}
						}
					}
				}
				outCh <- res{j.abs, m}
			}
		}()
	}
	go func() { wg.Wait(); close(outCh) }()

	result := make(map[string]fileMetadata, len(absPaths))
	for r := range outCh {
		result[r.abs] = r.meta
	}
	return result
}

// quickPkg extracts the Go package name from the first 512 bytes. Returns ""
// for non-Go or unparseable content.
func quickPkg(data []byte) string {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	for _, ln := range strings.SplitN(string(head), "\n", 20) {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		if strings.HasPrefix(t, "package ") {
			parts := strings.Fields(t)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
		break
	}
	return ""
}

// ── renderPathsTree (unchanged) ─────────────────────────────────────────────

// renderPathsTree renders a flat list of relative paths as an indented tree,
// inferring intermediate directories — so `glob` can return readable STRUCTURE
// instead of a wall of paths.
func renderPathsTree(paths []string) string {
	all := map[string]bool{} // rel -> isDir
	for _, p := range paths {
		p = strings.Trim(filepath.ToSlash(p), "/")
		if p == "" {
			continue
		}
		all[p] = false
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			all[strings.Join(parts[:i], "/")] = true
		}
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	key := func(s string) string { return strings.ReplaceAll(s, "/", "\x00") }
	sort.Slice(keys, func(i, j int) bool { return key(keys[i]) < key(keys[j]) })
	var b strings.Builder
	for _, k := range keys {
		depth := strings.Count(k, "/")
		name := k[strings.LastIndex(k, "/")+1:]
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString(name)
		if all[k] {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderPathsTreeRich renders glob matches as an indented tree enriched with
// per-file metadata (line count, package, top symbols). root is the workspace
// root used for repomap cache lookup; pass "" to skip metadata enrichment.
func renderPathsTreeRich(root string, paths []string) string {
	// Collect metadata in parallel for all file paths.
	var absPaths, relPaths []string
	all := map[string]bool{}
	for _, p := range paths {
		p = strings.Trim(filepath.ToSlash(p), "/")
		if p == "" {
			continue
		}
		all[p] = false
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			all[strings.Join(parts[:i], "/")] = true
		}
		if root != "" {
			absPaths = append(absPaths, filepath.Join(root, filepath.FromSlash(p)))
			relPaths = append(relPaths, p)
		}
	}
	meta := collectMetadata(root, absPaths, relPaths)
	// Build abs→rel reverse so we can look up by abs path.
	absToRel := make(map[string]string, len(absPaths))
	for i, abs := range absPaths {
		absToRel[abs] = relPaths[i]
	}

	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	keyFn := func(s string) string { return strings.ReplaceAll(s, "/", "\x00") }
	sort.Slice(keys, func(i, j int) bool { return keyFn(keys[i]) < keyFn(keys[j]) })

	var b strings.Builder
	for _, k := range keys {
		depth := strings.Count(k, "/")
		name := k[strings.LastIndex(k, "/")+1:]
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString(name)
		if all[k] {
			b.WriteByte('/')
		} else if root != "" {
			abs := filepath.Join(root, filepath.FromSlash(k))
			if m, ok := meta[abs]; ok {
				if m.pkg != "" {
					fmt.Fprintf(&b, "  [%s]", m.pkg)
				}
				if m.lines > 0 {
					fmt.Fprintf(&b, "  %dL", m.lines)
				}
				if len(m.topSyms) > 0 {
					fmt.Fprintf(&b, "  %s", strings.Join(m.topSyms, ", "))
				}
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ── renderDirOutline ────────────────────────────────────────────────────────

// renderDirOutline produces a cross-file structural map of a directory: every
// code file with its definitions + line numbers. Uses the repomap per-file cache
// when available (treesitter symbols with line numbers); falls back to the regex
// outliner for files not yet cached. Cap raised to 500 files.
func renderDirOutline(base string) string {
	const outlineCap = 500
	ignore := loadGitignore(base)
	var files []string
	_ = filepath.WalkDir(base, func(abs string, d os.DirEntry, err error) error {
		if err != nil || abs == base {
			return nil
		}
		rel, ok := relInside(base, abs)
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
				if r, ok := relUnder(base, abs); ok && ignore.ignored(r, true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if ignore != nil {
			if r, ok := relUnder(base, abs); ok && ignore.ignored(r, false) {
				return nil
			}
		}
		files = append(files, abs)
		return nil
	})
	sort.Strings(files)

	var b strings.Builder
	shown := 0
	for _, abs := range files {
		if shown >= outlineCap {
			fmt.Fprintf(&b, "\n… outline capped at %d files — read a subdirectory for the rest.\n", outlineCap)
			break
		}
		rel, _ := relInside(base, abs)
		relSlash := filepath.ToSlash(rel)

		// Prefer the repomap cache: has treesitter symbols with precise line numbers.
		if fs, ok := repomap.LookupFileSyms(base, relSlash); ok && len(fs.Syms) > 0 {
			pkg := ""
			if fs.Package != "" {
				pkg = " [" + fs.Package + "]"
			}
			fmt.Fprintf(&b, "%s%s:\n", relSlash, pkg)
			for _, s := range fs.Syms {
				if s.Sig != "" {
					fmt.Fprintf(&b, "  L%-4d %s\n", s.Line, s.Sig)
				}
			}
			b.WriteByte('\n')
			shown++
			continue
		}

		// Fallback: regex outline (works without treesitter build tag).
		data, _, err := readCapped(abs, dirOutlineFileCap)
		if err != nil || len(data) == 0 {
			continue
		}
		head := data
		if len(head) > 8192 {
			head = head[:8192]
		}
		if detectKind(head).kind != "text" {
			continue
		}
		om, n := outlineOf(string(data))
		if n == 0 {
			continue
		}
		pkg := quickPkg(data)
		pkgStr := ""
		if pkg != "" {
			pkgStr = " [" + pkg + "]"
		}
		fmt.Fprintf(&b, "%s%s:\n%s\n", relSlash, pkgStr, om)
		shown++
	}
	if shown == 0 {
		return "(no files with recognizable code structure here — read individual files)\n"
	}
	return b.String()
}

// ── renderDirTree ────────────────────────────────────────────────────────────

// renderDirTree builds a bounded, .gitignore-aware indented tree of a directory
// enriched with per-file metadata (line count, package, key symbols) and per-
// directory aggregate stats (file count + total lines). Used by `read .` to
// orient the agent without globbing.
func renderDirTree(base string) string {
	ignore := loadGitignore(base)

	type ent struct {
		rel   string
		abs   string
		isDir bool
	}
	var ents []ent
	truncated := false

	_ = filepath.WalkDir(base, func(abs string, d os.DirEntry, err error) error {
		if err != nil || abs == base {
			return nil
		}
		rel, ok := relInside(base, abs)
		if !ok || rel == "." {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if d.IsDir() {
			if isSkipped(d.Name(), abs) {
				return filepath.SkipDir
			}
			if ignore != nil {
				if r, ok := relUnder(base, abs); ok && ignore.ignored(r, true) {
					return filepath.SkipDir
				}
			}
			if depth >= treeMaxDepth {
				if len(ents) < treeMaxEntries {
					ents = append(ents, ent{rel: filepath.ToSlash(rel) + "/…"})
				}
				return filepath.SkipDir
			}
		} else {
			if ignore != nil {
				if r, ok := relUnder(base, abs); ok && ignore.ignored(r, false) {
					return nil
				}
			}
		}
		if len(ents) >= treeMaxEntries {
			truncated = true
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ents = append(ents, ent{
			rel:   filepath.ToSlash(rel),
			abs:   abs,
			isDir: d.IsDir(),
		})
		return nil
	})

	// Sort: directory immediately before its own children.
	key := func(s string) string { return strings.ReplaceAll(s, "/", "\x00") }
	sort.Slice(ents, func(i, j int) bool { return key(ents[i].rel) < key(ents[j].rel) })

	// Collect per-file metadata in parallel.
	var fileAbs, fileRel []string
	for _, e := range ents {
		if !e.isDir {
			fileAbs = append(fileAbs, e.abs)
			fileRel = append(fileRel, e.rel)
		}
	}
	meta := collectMetadata(base, fileAbs, fileRel)

	// Compute per-directory aggregate stats (file count + total lines).
	type dirStats struct{ files, lines int }
	dirAgg := map[string]*dirStats{}
	for _, e := range ents {
		if e.isDir {
			dirAgg[e.rel] = &dirStats{}
			continue
		}
		m := meta[e.abs]
		parts := strings.Split(e.rel, "/")
		for d := 1; d < len(parts); d++ {
			dirKey := strings.Join(parts[:d], "/")
			if ds, ok := dirAgg[dirKey]; ok {
				ds.files++
				ds.lines += m.lines
			}
		}
	}

	var b strings.Builder
	for _, e := range ents {
		depth := strings.Count(e.rel, "/")
		name := e.rel[strings.LastIndex(e.rel, "/")+1:]
		indent := strings.Repeat("  ", depth)

		if e.isDir {
			ds := dirAgg[e.rel]
			suffix := ""
			if ds != nil && ds.files > 0 {
				suffix = fmt.Sprintf("  (%d files · %d lines)", ds.files, ds.lines)
			}
			fmt.Fprintf(&b, "%s%s/%s\n", indent, name, suffix)
		} else {
			m := meta[e.abs]
			info := ""
			if m.pkg != "" {
				info += fmt.Sprintf("  [%s]", m.pkg)
			}
			if m.lines > 0 {
				info += fmt.Sprintf("  %dL", m.lines)
			}
			if len(m.topSyms) > 0 {
				info += "  " + strings.Join(m.topSyms, ", ")
			}
			fmt.Fprintf(&b, "%s%s%s\n", indent, name, info)
		}
	}

	out := b.String()
	if out == "" {
		out = "(empty directory)\n"
	}
	if truncated {
		out += fmt.Sprintf("\n… truncated at %d entries — read a subdirectory or use glob with a pattern to go deeper.\n", treeMaxEntries)
	}
	return out
}
