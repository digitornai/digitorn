package rag

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// SyncReport summarises one source sync run.
type SyncReport struct {
	Added, Updated, Deleted, Skipped, Chunks int
}

// SyncSource brings a KB in line with a file source : it stat-diffs the
// directory against the last run, re-ingests changed/new files (clean
// replace via DeleteBySource), and removes deleted files. Incremental
// within a process; cheap (mtime+size change detection, no content read
// for unchanged files). Caller runs it off the daemon loop.
func (e *Engine) SyncSource(ctx context.Context, src SourceConfig) (SyncReport, error) {
	if t := strings.ToLower(src.Type); t != "" && t != "file" {
		return SyncReport{}, fmt.Errorf("rag: source type %q not supported yet", src.Type)
	}
	if strings.TrimSpace(src.Path) == "" {
		return SyncReport{}, fmt.Errorf("rag: source path required")
	}
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	recursive := src.Recursive == nil || *src.Recursive
	maxFiles := src.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 10000
	}
	allow := map[string]bool{}
	for _, x := range src.Extensions {
		allow[strings.ToLower(x)] = true
	}

	root := filepath.Clean(src.Path)
	present := map[string]string{} // relpath -> "mtime:size"
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if !recursive && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if len(present) >= maxFiles {
			return fs.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !SupportedExt(ext) || (len(allow) > 0 && !allow[ext]) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		present[rel] = fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
		return nil
	})

	key := kb + "\x00" + root
	e.mu.Lock()
	prev := e.srcState[key]
	e.mu.Unlock()
	if prev == nil {
		prev = map[string]string{}
	}

	var rep SyncReport
	changed := false
	for rel, sig := range present {
		if prev[rel] == sig {
			continue
		}
		loaded, err := LoadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			rep.Skipped++
			continue
		}
		_ = e.backend.DeleteBySource(ctx, kb, rel) // clean replace
		n, err := e.Ingest(ctx, kb, loaded.Text, rel)
		if err != nil {
			return rep, err
		}
		if prev[rel] == "" {
			rep.Added++
		} else {
			rep.Updated++
		}
		rep.Chunks += n
		changed = true
	}
	for rel := range prev {
		if _, ok := present[rel]; ok {
			continue
		}
		_ = e.backend.DeleteBySource(ctx, kb, rel)
		rep.Deleted++
		changed = true
	}

	e.mu.Lock()
	e.srcState[key] = present
	if changed {
		delete(e.idx, kb) // force keyword-index rebuild from the now-correct backend
	}
	e.mu.Unlock()
	return rep, nil
}

// SyncAll syncs every configured source. Returns the combined report.
func (e *Engine) SyncAll(ctx context.Context) (SyncReport, error) {
	var total SyncReport
	for _, src := range e.cfg.Sources {
		r, err := e.SyncSource(ctx, src)
		if err != nil {
			return total, err
		}
		total.Added += r.Added
		total.Updated += r.Updated
		total.Deleted += r.Deleted
		total.Skipped += r.Skipped
		total.Chunks += r.Chunks
	}
	return total, nil
}
