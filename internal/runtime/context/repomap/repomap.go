package repomap

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Sym struct {
	Key  string
	Name string
	Kind string
	File string
	Sig  string
	Line int
}

type Graph struct {
	Syms  []Sym
	Calls map[string][]string
}

const (
	maxRoots    = 4
	ttl         = 5 * time.Minute
	budgetChars = 6000
	// firstBuildWait bounds how long the FIRST repo-map build may block the turn
	// that triggered it, so the opening turn gets the map instead of a blank. A
	// repo that overruns it keeps building in the background and lands next turn.
	firstBuildWait = 4 * time.Second
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
		file  string
		score float64
		syms  []int
	}
	idxByFile := map[string]int{}
	var files []fileEntry
	for i, s := range g.Syms {
		fi, ok := idxByFile[s.File]
		if !ok {
			fi = len(files)
			idxByFile[s.File] = fi
			files = append(files, fileEntry{file: s.File})
		}
		files[fi].score += rank[i]
		files[fi].syms = append(files[fi].syms, i)
	}
	for fi := range files {
		syms := files[fi].syms
		sort.SliceStable(syms, func(a, b int) bool {
			ia, ib := syms[a], syms[b]
			if rank[ia] != rank[ib] {
				return rank[ia] > rank[ib]
			}
			return g.Syms[ia].Line < g.Syms[ib].Line
		})
	}
	sort.SliceStable(files, func(a, b int) bool {
		if files[a].score != files[b].score {
			return files[a].score > files[b].score
		}
		return files[a].file < files[b].file
	})

	var b strings.Builder
	b.WriteString("Ranked map of the most important symbols in this codebase (signatures only; use grep/read for bodies):\n")
	used := b.Len()
	for _, fe := range files {
		header := "\n" + fe.file + ":\n"
		if used+len(header) > budget {
			break
		}
		b.WriteString(header)
		used += len(header)
		for _, si := range fe.syms {
			line := "  " + g.Syms[si].Sig + "\n"
			if used+len(line) > budget {
				break
			}
			b.WriteString(line)
			used += len(line)
		}
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
}

var (
	mgrMu    sync.Mutex
	entries  = map[string]*entry{}
	provider func(root string) Graph
)

func Register(fn func(root string) Graph) {
	mgrMu.Lock()
	provider = fn
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
		if strings.HasPrefix(filepathToSlash(absPath), filepathToSlash(e.root)) {
			e.mu.Lock()
			e.dirty = true
			e.mu.Unlock()
		}
	}
}

func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

func Get(root string) string {
	if root == "" {
		return ""
	}
	mgrMu.Lock()
	p := provider
	if p == nil {
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
		build := func() {
			defer func() {
				recover()
				e.mu.Lock()
				e.building = false
				e.mu.Unlock()
			}()
			s := Render(p(e.root), budgetChars)
			e.mu.Lock()
			e.rendered = s
			e.ready = true
			e.dirty = false
			e.builtAt = time.Now()
			e.mu.Unlock()
		}
		if out == "" {
			// FIRST build, nothing cached yet : briefly WAIT so the very first turn
			// gets the map instead of a blank (the turn that matters most for
			// orientation). Bounded — a huge repo that overruns the wait just keeps
			// building in the background and lands on the next turn, never hanging.
			done := make(chan struct{})
			go func() { build(); close(done) }()
			select {
			case <-done:
			case <-time.After(firstBuildWait):
			}
			e.mu.Lock()
			out = e.rendered
			e.mu.Unlock()
		} else {
			go build() // stale refresh : serve the cached map now, refresh off-turn
		}
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
