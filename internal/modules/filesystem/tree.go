package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	treeMaxEntries = 400
	treeMaxDepth   = 5

	dirOutlineMaxFiles = 80
	dirOutlineFileCap  = 512 << 10
)

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

// renderDirOutline produces a cross-file structural map of a directory: every
// code file with its definitions (the regex outliner — no treesitter, so it
// works in every build). Bounded so a large tree stays digestible. With the
// tree, this lets the agent grasp a project without globbing+reading file by file.
func renderDirOutline(base string) string {
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
			if _, skip := skipDirs[d.Name()]; skip {
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
		if shown >= dirOutlineMaxFiles {
			fmt.Fprintf(&b, "\n… outline capped at %d files — read a subdirectory for the rest.\n", dirOutlineMaxFiles)
			break
		}
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
		rel, _ := relInside(base, abs)
		fmt.Fprintf(&b, "%s:\n%s\n", filepath.ToSlash(rel), om)
		shown++
	}
	if shown == 0 {
		return "(no files with recognizable code structure here — read individual files)\n"
	}
	return b.String()
}

// renderDirTree builds a bounded, .gitignore-aware indented tree of a directory
// so `read` on a directory returns a project OVERVIEW (structure) instead of an
// error. Pure Go — no treesitter — so it works in every build, including the
// Linux/prod one. Directories come with a trailing "/" and group their children.
func renderDirTree(base string) string {
	ignore := loadGitignore(base)
	type ent struct {
		rel   string
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
			if _, skip := skipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			if ignore != nil {
				if r, ok := relUnder(base, abs); ok && ignore.ignored(r, true) {
					return filepath.SkipDir
				}
			}
			if depth >= treeMaxDepth {
				if len(ents) < treeMaxEntries {
					ents = append(ents, ent{rel: filepath.ToSlash(rel) + "/…", isDir: false})
				}
				return filepath.SkipDir
			}
		} else if ignore != nil {
			if r, ok := relUnder(base, abs); ok && ignore.ignored(r, false) {
				return nil
			}
		}
		if len(ents) >= treeMaxEntries {
			truncated = true
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ents = append(ents, ent{rel: filepath.ToSlash(rel), isDir: d.IsDir()})
		return nil
	})

	// Sort so a directory immediately precedes its own children (treat the path
	// separator as the lowest-ordering byte): a/, a/b.go, a.go — a proper tree.
	key := func(s string) string { return strings.ReplaceAll(s, "/", "\x00") }
	sort.Slice(ents, func(i, j int) bool { return key(ents[i].rel) < key(ents[j].rel) })

	var b strings.Builder
	for _, e := range ents {
		name := e.rel
		depth := strings.Count(e.rel, "/")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString(name)
		if e.isDir {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
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
