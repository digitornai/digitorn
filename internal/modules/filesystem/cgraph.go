//go:build treesitter

package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/codeast"
)

func astChunks(path string, src []byte) []sChunk {
	chs := codeast.Chunks(path, src)
	if len(chs) == 0 {
		return nil
	}
	out := make([]sChunk, 0, len(chs))
	for _, c := range chs {
		lines := strings.Count(c.Text, "\n") + 1
		out = append(out, sChunk{path: c.Path, line: c.Line, end: c.Line + lines - 1, text: c.Text, sym: c.Symbol})
	}
	return out
}

type defLoc struct {
	Name, Kind, Path string
	Start, End       int
}

type codeGraph struct {
	byFile  map[string][]defLoc
	callers map[string][]string
	imports map[string][]string
}

func (g *codeGraph) context(path string, line int) symContext {
	var sc symContext
	sc.Imports = g.imports[path]
	best := -1
	bestSpan := 1 << 30
	defs := g.byFile[path]
	for i, d := range defs {
		if line >= d.Start && line <= d.End {
			if span := d.End - d.Start; span < bestSpan {
				best, bestSpan = i, span
			}
		}
	}
	if best >= 0 {
		d := defs[best]
		sc.Symbol = strings.TrimSpace(d.Kind + " " + d.Name)
		direct := dedupStrings(g.callers[d.Name], 8)
		sc.Callers = direct

		func() {
			defer func() { recover() }()
			seen := make(map[string]bool, len(direct))
			for _, c := range direct {
				seen[c] = true
			}
			var hop2 []string
			for _, c := range direct {
				name := callerSymName(c)
				if name == "" {
					continue
				}
				for _, cc := range g.callers[name] {
					if !seen[cc] {
						seen[cc] = true
						hop2 = append(hop2, cc)
						if len(hop2) >= 5 {
							return
						}
					}
				}
			}
			if len(hop2) > 0 {
				sc.Callers = append(sc.Callers, "↳ "+strings.Join(hop2, ", "))
			}
		}()
	}
	return sc
}

func callerSymName(label string) string {
	parts := strings.Fields(label)
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if i := strings.LastIndexByte(last, '.'); i >= 0 {
		return last[i+1:]
	}
	return last
}

type cgEntry struct {
	root     string
	maxBytes int64
	mu       sync.Mutex
	building bool
	ready    bool
	g        *codeGraph
	builtAt  time.Time
	usedAt   time.Time
	dirty    bool
}

type cgManager struct {
	mu     sync.Mutex
	byRoot map[string]*cgEntry
}

var cgraphs = &cgManager{byRoot: map[string]*cgEntry{}}

func (m *cgManager) get(root string, maxBytes int64) *cgEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byRoot[root]; ok {
		return e
	}
	if len(m.byRoot) >= sindexMaxRoots {
		var oldestKey string
		var oldest time.Time
		for k, e := range m.byRoot {
			e.mu.Lock()
			u := e.usedAt
			e.mu.Unlock()
			if oldestKey == "" || u.Before(oldest) {
				oldestKey, oldest = k, u
			}
		}
		if oldestKey != "" {
			delete(m.byRoot, oldestKey)
		}
	}
	e := &cgEntry{root: root, maxBytes: maxBytes}
	m.byRoot[root] = e
	return e
}

func (m *cgManager) markDirty(abs string) {
	m.mu.Lock()
	es := make([]*cgEntry, 0, len(m.byRoot))
	for _, e := range m.byRoot {
		es = append(es, e)
	}
	m.mu.Unlock()
	for _, e := range es {
		if underRoot(e.root, abs) {
			e.mu.Lock()
			e.dirty = true
			e.mu.Unlock()
		}
	}
}

func (e *cgEntry) maybeBuild() {
	e.mu.Lock()
	stale := e.dirty || (e.ready && time.Since(e.builtAt) > sindexTTL)
	if e.building || (e.ready && !stale) {
		e.mu.Unlock()
		return
	}
	e.building = true
	e.mu.Unlock()
	go func() {
		defer func() {
			recover()
			e.mu.Lock()
			e.building = false
			e.mu.Unlock()
		}()
		g := buildGraph(e.root, e.maxBytes)
		e.mu.Lock()
		e.g = g
		e.ready = true
		e.dirty = false
		e.builtAt = time.Now()
		e.mu.Unlock()
	}()
}

func (e *cgEntry) context(path string, line int) symContext {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready || e.g == nil {
		return symContext{}
	}
	e.usedAt = time.Now()
	return e.g.context(path, line)
}

func buildGraph(root string, maxBytes int64) *codeGraph {
	g := &codeGraph{byFile: map[string][]defLoc{}, callers: map[string][]string{}, imports: map[string][]string{}}

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
		if !codeast.Supported(filepath.Ext(path)) {
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
		fp  codeast.FileParse
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
				if fp, ok := codeast.ParseFile(rel, b); ok {
					results <- result{rel: rel, fp: fp}
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	for r := range results {
		if len(r.fp.Imports) > 0 {
			g.imports[r.rel] = r.fp.Imports
		}
		for _, s := range r.fp.Syms {
			g.byFile[r.rel] = append(g.byFile[r.rel], defLoc{Name: s.Name, Kind: s.Kind, Path: r.rel, Start: s.Start, End: s.End})
			label := strings.TrimSpace(s.Kind + " " + s.Name + " @" + r.rel)
			for _, callee := range s.Calls {
				g.callers[callee] = append(g.callers[callee], label)
			}
		}
	}
	return g
}

func codeContextFor(root string, maxBytes int64, path string, line int) symContext {
	e := cgraphs.get(root, maxBytes)
	e.maybeBuild()
	return e.context(path, line)
}

func dedupStrings(in []string, max int) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}
