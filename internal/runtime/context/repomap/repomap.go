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

type Sym struct {
	Key     string
	Name    string
	Kind    string
	File    string
	Sig     string
	Fields  string
	Package string
	Line    int
	EndLine int
}

type Graph struct {
	Syms  []Sym
	Calls map[string][]string
}

type WalkEntry struct {
	Abs     string
	Rel     string
	ModTime time.Time
	Size    int64
}

type FileSyms struct {
	Package string
	Syms    []Sym
	Calls   map[string][]string
}

type fileCacheEntry struct {
	ModTime time.Time
	FileSyms
}

const (
	maxRoots    = 4
	ttl         = 30 * time.Second
	budgetChars = 3000
)

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

func Render(g Graph, budget int) string {
	n := len(g.Syms)
	if n == 0 {
		return ""
	}
	if budget <= 0 {
		budget = budgetChars
	}

	byKey := make(map[string]int, n)
	byName := make(map[string][]int, n)
	for i, s := range g.Syms {
		byKey[s.Key] = i
		byName[s.Name] = append(byName[s.Name], i)
	}
	adj := make([][]int, n)
	for i, s := range g.Syms {
		seen := map[int]bool{}
		for _, callee := range g.Calls[s.Key] {
			for _, j := range byName[callee] {
				if j != i && !seen[j] {
					seen[j] = true
					adj[i] = append(adj[i], j)
				}
			}
		}
	}
	rank := pagerank(adj, 20, 0.85)

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

	sort.SliceStable(files, func(a, b int) bool {
		if files[a].score != files[b].score {
			return files[a].score > files[b].score
		}
		return files[a].file < files[b].file
	})

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

	var b strings.Builder

	b.WriteString(fmt.Sprintf("# %d files · %d symbols\n", len(files), n))
	for _, fe := range files {
		line := fe.file
		if fe.pkg != "" {
			line += " [" + fe.pkg + "]"
		}
		b.WriteString(line + "\n")
	}
	used := b.Len()

	symBudget := budget - used
	if symBudget <= 0 {
		return strings.TrimRight(b.String(), "\n")
	}
	numFiles := len(files)
	if numFiles == 0 {
		numFiles = 1
	}
	perFileCap := symBudget / 4
	if perFileCap < 300 {
		perFileCap = 300
	}

	b.WriteString("\n")
	used++

	for _, fe := range files {
		if used >= budget {
			break
		}
		fileHdr := "## " + fe.file + "\n"
		if used+len(fileHdr) > budget {
			break
		}

		var fileBuf strings.Builder
		fileBuf.WriteString(fileHdr)
		fileUsed := len(fileHdr)

		for _, si := range fe.symIdxs {
			s := &g.Syms[si]
			if s.Sig == "" {
				continue
			}
			lineRef := fmt.Sprintf("L%d", s.Line)
			if s.EndLine > s.Line {
				lineRef = fmt.Sprintf("L%d-L%d", s.Line, s.EndLine)
			}
			sigLine := fmt.Sprintf("  %-14s %s\n", lineRef, s.Sig)
			if fileUsed+len(sigLine) > perFileCap {
				break
			}
			fileBuf.WriteString(sigLine)
			fileUsed += len(sigLine)
		}

		chunk := fileBuf.String()
		if used+len(chunk) > budget {
			break
		}
		b.WriteString(chunk)
		used += len(chunk)
	}

	return strings.TrimRight(b.String(), "\n")
}

type entry struct {
	root     string
	mu       sync.Mutex
	building bool
	ready    bool
	rendered string
	builtAt  time.Time
	usedAt   time.Time
	dirty    bool

	files map[string]fileCacheEntry
}

func (e *entry) buildIncremental(
	walk func(root string) []WalkEntry,
	parse func(rel string, content []byte) (FileSyms, bool),
) {
	current := walk(e.root)

	e.mu.Lock()
	if e.files == nil {
		e.files = make(map[string]fileCacheEntry, len(current))
	}
	cachedSnapshot := make(map[string]fileCacheEntry, len(e.files))
	for k, v := range e.files {
		cachedSnapshot[k] = v
	}
	e.mu.Unlock()

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

	e.mu.Lock()
	for rel := range e.files {
		if _, ok := seen[rel]; !ok {
			delete(e.files, rel)
		}
	}
	for _, r := range parsed {
		if r.ok {
			e.files[r.rel] = fileCacheEntry{ModTime: r.mod, FileSyms: r.fs}
		}
	}
	fileCopy := make(map[string]fileCacheEntry, len(e.files))
	for k, v := range e.files {
		fileCopy[k] = v
	}
	e.mu.Unlock()

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

var (
	mgrMu    sync.Mutex
	entries  = map[string]*entry{}
	provider func(root string) Graph
	walkerFn func(root string) []WalkEntry
	parserFn func(rel string, content []byte) (FileSyms, bool)
)

func Register(fn func(root string) Graph) {
	mgrMu.Lock()
	provider = fn
	mgrMu.Unlock()
}

func RegisterIncremental(
	walk func(root string) []WalkEntry,
	parse func(rel string, content []byte) (FileSyms, bool),
) {
	mgrMu.Lock()
	walkerFn = walk
	parserFn = parse
	mgrMu.Unlock()
}

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
