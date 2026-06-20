package repomap

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Sym is one symbol extracted from a source file.
type Sym struct {
	Key     string
	Name    string
	Kind    string
	File    string
	Sig     string
	Fields  string // first 2-3 lines of a struct/interface body, for richer display
	Package string // Go package name (or lang equivalent)
	Line    int    // 1-based start line
	EndLine int    // 1-based end line (0 = unknown); enables edit(start_line, end_line) without a read
}

// Graph is a set of symbols with call edges between them.
type Graph struct {
	Syms  []Sym
	Calls map[string][]string // sym key → slice of callee symbol names
}

// WalkEntry is one file discovered during a directory walk.
type WalkEntry struct {
	Abs     string
	Rel     string // workspace-relative slash path
	ModTime time.Time
	Size    int64
}

// FileSyms is the treesitter (or regex) parse result for one file.
type FileSyms struct {
	Package string // package / module name of this file
	Syms    []Sym
	Calls   map[string][]string // sym key → callee names
}

// fileCacheEntry pairs a file's parse result with the mtime it was parsed at.
type fileCacheEntry struct {
	ModTime time.Time
	FileSyms
}

const (
	maxRoots    = 4
	ttl         = 30 * time.Second // short fallback for changes outside the filesystem module (bash writes, git ops)
	// budgetChars is generous: the agent needs file paths + line numbers for
	// every key symbol so it can jump directly with Read(path, offset=N) and
	// Edit without grep/glob. At ~4 chars/token this costs ~10k tokens in the
	// system prompt — well within 128k+ context windows.
	budgetChars = 40000
)

// ── PageRank ────────────────────────────────────────────────────────────────

func pagerank(adj [][]int, iters int, damp float64) []float64 {
	n := len(adj)
	rank := make([]float64, n)
	if n == 0 {
		return rank
	}
	out := make([]float64, n)
	for i := range adj {
		out[i] = float64(len(adj[i]))
		rank[i] = 1.0 / float64(n)
	}
	tmp := make([]float64, n)
	for it := 0; it < iters; it++ {
		var dangling float64
		for i := 0; i < n; i++ {
			if out[i] == 0 {
				dangling += rank[i]
			}
		}
		base := (1-damp)/float64(n) + damp*dangling/float64(n)
		for i := range tmp {
			tmp[i] = base
		}
		for i := 0; i < n; i++ {
			if out[i] == 0 {
				continue
			}
			share := damp * rank[i] / out[i]
			for _, j := range adj[i] {
				tmp[j] += share
			}
		}
		copy(rank, tmp)
	}
	return rank
}

// ── Render ──────────────────────────────────────────────────────────────────

// Render turns a Graph into a rich LLM-readable codebase map ranked by
// PageRank centrality.
//
// Format (per file, sorted by aggregate score):
//
//	## internal/runtime/engine.go  [runtime]
//	  func (e *Engine) Run(ctx context.Context, in TurnInput) (*TurnResult, error)  ⬆12
//	  type Engine struct
//	    Apps AppLookup
//	    Sessions SessionAccess
func Render(g Graph, budget int) string {
	n := len(g.Syms)
	if n == 0 {
		return ""
	}
	if budget <= 0 {
		budget = budgetChars
	}

	// Build adjacency for PageRank (callee edges) and reverse for caller counts.
	byKey := make(map[string]int, n)
	byName := make(map[string][]int, n)
	for i, s := range g.Syms {
		byKey[s.Key] = i
		byName[s.Name] = append(byName[s.Name], i)
	}
	adj := make([][]int, n)
	inCount := make([]int, n) // how many symbols call this one
	for i, s := range g.Syms {
		seen := map[int]bool{}
		for _, callee := range g.Calls[s.Key] {
			for _, j := range byName[callee] {
				if j != i && !seen[j] {
					seen[j] = true
					adj[i] = append(adj[i], j)
					inCount[j]++
				}
			}
		}
	}
	rank := pagerank(adj, 20, 0.85)

	// Determine "hot" threshold: top-20% by inCount.
	maxIn := 0
	for _, c := range inCount {
		if c > maxIn {
			maxIn = c
		}
	}
	hotThreshold := 1
	if maxIn > 4 {
		hotThreshold = maxIn / 5
	}

	// Group symbols by file, accumulate file score.
	type fileEntry struct {
		file    string
		pkg     string
		score   float64
		symIdxs []int
	}
	idxByFile := map[string]int{}
	var files []fileEntry
	for i, s := range g.Syms {
		fi, ok := idxByFile[s.File]
		if !ok {
			fi = len(files)
			idxByFile[s.File] = fi
			files = append(files, fileEntry{file: s.File, pkg: s.Package})
		}
		files[fi].score += rank[i]
		files[fi].symIdxs = append(files[fi].symIdxs, i)
	}

	// Sort symbols within each file by rank desc, then line asc.
	for fi := range files {
		idxs := files[fi].symIdxs
		sort.SliceStable(idxs, func(a, b int) bool {
			ia, ib := idxs[a], idxs[b]
			if rank[ia] != rank[ib] {
				return rank[ia] > rank[ib]
			}
			return g.Syms[ia].Line < g.Syms[ib].Line
		})
	}
	// Sort files by score desc, then path asc.
	sort.SliceStable(files, func(a, b int) bool {
		if files[a].score != files[b].score {
			return files[a].score > files[b].score
		}
		return files[a].file < files[b].file
	})

	// Render.
	var b strings.Builder
	header := fmt.Sprintf(
		"# CODEBASE INDEX — %d files · %d symbols\n"+
			"# Each symbol shows its line number: Read(path, offset=N) jumps directly. No grep needed.\n"+
			"# ⬆N = called by N other symbols (centrality). Ranked: most important files first.\n",
		len(files), n)
	b.WriteString(header)
	used := b.Len()

	for _, fe := range files {
		// File header: path + package name.
		pkg := ""
		if fe.pkg != "" {
			pkg = "  [" + fe.pkg + "]"
		}
		fileHdr := "\n## " + fe.file + pkg + "\n"
		if used+len(fileHdr) > budget {
			break
		}
		b.WriteString(fileHdr)
		used += len(fileHdr)

		for _, si := range fe.symIdxs {
			s := &g.Syms[si]
			if s.Sig == "" {
				continue
			}
			// Caller count — suppress for generic names (appear in many files).
			callerSuffix := ""
			if inCount[si] >= hotThreshold && inCount[si] > 0 && inCount[si] <= n/2 {
				callerSuffix = fmt.Sprintf("  ⬆%d", inCount[si])
			}
			// Line range prefix: L302-L450 for multi-line symbols, L302 for single-liners.
			// The end line enables edit(start_line, end_line) without a prior read.
			lineRef := fmt.Sprintf("L%d", s.Line)
			if s.EndLine > s.Line {
				lineRef = fmt.Sprintf("L%d-L%d", s.Line, s.EndLine)
			}
			sigLine := fmt.Sprintf("  %-12s %s%s\n", lineRef, s.Sig, callerSuffix)

			// For struct/interface: show first 2-3 fields indented.
			fieldLines := ""
			if s.Fields != "" {
				for _, fl := range strings.SplitN(s.Fields, "\n", 4) {
					fl = strings.TrimSpace(fl)
					if fl == "" || fl == "{" || fl == "}" {
						continue
					}
					fieldLines += "         " + fl + "\n"
				}
			}

			cost := len(sigLine) + len(fieldLines)
			if used+cost > budget {
				break
			}
			b.WriteString(sigLine)
			if fieldLines != "" {
				b.WriteString(fieldLines)
			}
			used += cost
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── Cache entry ─────────────────────────────────────────────────────────────

type entry struct {
	root     string
	mu       sync.Mutex
	building bool
	ready    bool
	rendered string
	builtAt  time.Time
	usedAt   time.Time
	dirty    bool

	// Per-file parse cache keyed by workspace-relative slash path.
	// Only files whose mtime changed are re-parsed on a stale refresh.
	files map[string]fileCacheEntry
}

// buildIncremental walks the workspace, re-parses only new/changed files,
// assembles the full graph from the cache, and writes the rendered result.
// Must only be called from a single goroutine at a time (guarded by e.building).
func (e *entry) buildIncremental(
	walk func(root string) []WalkEntry,
	parse func(rel string, content []byte) (FileSyms, bool),
) {
	current := walk(e.root)

	e.mu.Lock()
	if e.files == nil {
		e.files = make(map[string]fileCacheEntry, len(current))
	}
	// Snapshot the file cache so we can diff without holding the lock.
	cachedSnapshot := make(map[string]fileCacheEntry, len(e.files))
	for k, v := range e.files {
		cachedSnapshot[k] = v
	}
	e.mu.Unlock()

	// Find new/modified files.
	type job struct {
		abs string
		rel string
		mod time.Time
	}
	var jobs []job
	seen := make(map[string]struct{}, len(current))
	for _, f := range current {
		seen[f.Rel] = struct{}{}
		if cached, ok := cachedSnapshot[f.Rel]; !ok || !cached.ModTime.Equal(f.ModTime) {
			jobs = append(jobs, job{f.Abs, f.Rel, f.ModTime})
		}
	}

	// Parse new/modified files in parallel (without holding e.mu).
	type result struct {
		rel string
		mod time.Time
		fs  FileSyms
		ok  bool
	}
	var parsed []result
	if len(jobs) > 0 {
		nw := runtime.NumCPU()
		if nw > len(jobs) {
			nw = len(jobs)
		}
		jobsCh := make(chan job, len(jobs))
		for _, j := range jobs {
			jobsCh <- j
		}
		close(jobsCh)
		resultsCh := make(chan result, nw*2)
		var wg sync.WaitGroup
		for w := 0; w < nw; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobsCh {
					b, err := os.ReadFile(j.abs)
					if err != nil || !utf8.Valid(b) {
						continue
					}
					fs, ok := parse(j.rel, b)
					resultsCh <- result{j.rel, j.mod, fs, ok}
				}
			}()
		}
		go func() { wg.Wait(); close(resultsCh) }()
		for r := range resultsCh {
			parsed = append(parsed, r)
		}
	}

	// Update the file cache under the lock.
	e.mu.Lock()
	// Remove deleted files.
	for rel := range e.files {
		if _, ok := seen[rel]; !ok {
			delete(e.files, rel)
		}
	}
	// Store new/updated parse results.
	for _, r := range parsed {
		if r.ok {
			e.files[r.rel] = fileCacheEntry{ModTime: r.mod, FileSyms: r.fs}
		}
		// If not parseable: remove from cache so it gets retried next build.
		// (If a previous entry existed, the mtime mismatch will keep triggering
		// retries — correct, since the file changed to something unparseable.)
	}
	// Take a local copy of the whole file cache for graph assembly.
	fileCopy := make(map[string]fileCacheEntry, len(e.files))
	for k, v := range e.files {
		fileCopy[k] = v
	}
	e.mu.Unlock()

	// Assemble graph from all cached files (no lock needed — fileCopy is local).
	g := Graph{Calls: make(map[string][]string)}
	for _, fe := range fileCopy {
		g.Syms = append(g.Syms, fe.Syms...)
		for k, calls := range fe.Calls {
			g.Calls[k] = calls
		}
	}

	rendered := Render(g, budgetChars)

	e.mu.Lock()
	e.rendered = rendered
	e.ready = true
	e.dirty = false
	e.builtAt = time.Now()
	e.mu.Unlock()
}

// ── Global registry ─────────────────────────────────────────────────────────

var (
	mgrMu    sync.Mutex
	entries  = map[string]*entry{}
	provider func(root string) Graph   // legacy full-graph provider
	walkerFn func(root string) []WalkEntry
	parserFn func(rel string, content []byte) (FileSyms, bool)
)

// Register sets a full-graph provider (legacy, used when treesitter is absent).
func Register(fn func(root string) Graph) {
	mgrMu.Lock()
	provider = fn
	mgrMu.Unlock()
}

// RegisterIncremental registers per-file walker and parser functions.
// The repomap package owns the mtime cache and incremental graph assembly.
// When both are set, incremental mode is used over the legacy provider.
func RegisterIncremental(
	walk func(root string) []WalkEntry,
	parse func(rel string, content []byte) (FileSyms, bool),
) {
	mgrMu.Lock()
	walkerFn = walk
	parserFn = parse
	mgrMu.Unlock()
}

// MarkDirty invalidates the per-file cache entry for absPath so the next
// build re-parses only that file (all other files use their cached parse).
func MarkDirty(absPath string) {
	mgrMu.Lock()
	es := make([]*entry, 0, len(entries))
	for _, e := range entries {
		es = append(es, e)
	}
	mgrMu.Unlock()
	for _, e := range es {
		rel, ok := relUnder(e.root, absPath)
		if !ok {
			continue
		}
		e.mu.Lock()
		if e.files != nil {
			delete(e.files, filepath.ToSlash(rel))
		}
		e.dirty = true
		e.mu.Unlock()
	}
}

// relUnder returns the path of abs relative to root (lexical, no symlink
// resolution). Returns ok=false when abs is not under root.
func relUnder(root, abs string) (string, bool) {
	if root == "" {
		return "", false
	}
	r, err := filepath.Rel(root, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return r, true
}

func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// Get returns the latest rendered codebase map for root. The build always
// runs in the background (never blocks the caller). Returns "" when no
// build has completed yet; the result lands on the next call after completion.
func Get(root string) string {
	if root == "" {
		return ""
	}
	mgrMu.Lock()
	wfn := walkerFn
	pfn := parserFn
	pvd := provider
	if wfn == nil && pvd == nil {
		mgrMu.Unlock()
		return ""
	}
	e := entries[root]
	if e == nil {
		if len(entries) >= maxRoots {
			evictOldestLocked()
		}
		e = &entry{root: root}
		entries[root] = e
	}
	mgrMu.Unlock()

	e.mu.Lock()
	stale := e.dirty || (e.ready && time.Since(e.builtAt) > ttl)
	kick := !e.building && (!e.ready || stale)
	if kick {
		e.building = true
	}
	e.usedAt = time.Now()
	out := e.rendered
	e.mu.Unlock()

	if kick {
		go func() {
			defer func() {
				recover()
				e.mu.Lock()
				e.building = false
				e.mu.Unlock()
			}()
			if wfn != nil && pfn != nil {
				e.buildIncremental(wfn, pfn)
			} else {
				s := Render(pvd(e.root), budgetChars)
				e.mu.Lock()
				e.rendered = s
				e.ready = true
				e.dirty = false
				e.builtAt = time.Now()
				e.mu.Unlock()
			}
		}()
	}
	return out
}

func evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range entries {
		e.mu.Lock()
		u := e.usedAt
		e.mu.Unlock()
		if oldestKey == "" || u.Before(oldest) {
			oldestKey, oldest = k, u
		}
	}
	if oldestKey != "" {
		delete(entries, oldestKey)
	}
}

// LookupFileSyms returns the cached parse result for a workspace-relative slash
// path under root. Returns ok=false when the file is not in the cache yet (build
// hasn't completed or the file is not parseable). Never triggers a build.
func LookupFileSyms(root, rel string) (FileSyms, bool) {
	mgrMu.Lock()
	e := entries[root]
	mgrMu.Unlock()
	if e == nil {
		return FileSyms{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.files == nil {
		return FileSyms{}, false
	}
	fe, ok := e.files[filepath.ToSlash(rel)]
	if !ok {
		return FileSyms{}, false
	}
	return fe.FileSyms, true
}
