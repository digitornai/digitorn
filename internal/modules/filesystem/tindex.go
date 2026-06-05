package filesystem

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	csindex "github.com/mbathepaul/digitorn/internal/csearch/index"
	csregexp "github.com/mbathepaul/digitorn/internal/csearch/regexp"
)

// tindex.go : the trigram-index layer that turns grep from an O(corpus) rescan
// into an O(matches) candidate lookup. Per workspace root it builds a Russ-Cox /
// Google-Code-Search trigram index (vendored at internal/csearch), then a grep
// resolves the regexp to a trigram query and asks the index for the handful of
// files that *could* match — the parallel scanner then confirms only those.
//
// Reliability is non-negotiable, so every codesearch call sits behind recover()
// (a corrupt/truncated index panics in the vendored lib instead of os.Exit) and
// any failure degrades silently to a full scan — never a wrong or empty result.
//
// Correctness over a live tree is guaranteed by three always-scanned sets unioned
// with the trigram candidates :
//   - index hits     : files whose trigrams satisfy the query (the fast path),
//   - unindexed set  : files the indexer skipped (invalid UTF-8, huge lines …),
//     discovered by diffing intended-vs-actual indexed names after a build,
//   - dirty set      : files written/edited since the build read them.
// Every file grep would have scanned is therefore in exactly one of these, so an
// indexed search returns the same matches a full scan would — just far faster.

const (
	tindexTTL          = 10 * time.Minute // rebuild after this age regardless of churn
	tindexDirtyFloor   = 64               // rebuild once this many files changed …
	tindexDirtyPercent = 4                // … or churn exceeds nfiles/this
	tindexMaxRoots     = 32               // cap distinct workspace indexes kept hot
)

type tindexState int

const (
	tsIdle  tindexState = iota // no usable index yet
	tsReady                    // an index is mmapped and queryable
)

// tindex is one workspace root's index : its mmapped reader, the auxiliary
// always-scan sets, and the build state machine. All mutable fields are guarded
// by mu ; the mmapped *csindex.Index is immutable once published, so queries read
// it under mu only to keep a rebuild from unmapping it mid-read.
type tindex struct {
	root     string
	file     string // index file PREFIX, OUTSIDE any workspace (temp cache dir)
	maxBytes int64  // skip files larger than this (scanner skips them too)

	mu        sync.Mutex
	state     tindexState
	building  bool                // a (re)build is in flight ; old index keeps serving
	ix        *csindex.Index      // nil until first build publishes
	openPath  string              // on-disk path backing ix (deleted when superseded)
	gen       int                 // monotonic build generation → unique file per build
	nfiles    int                 // indexed file count (for the churn threshold)
	builtAt   time.Time           // publish time (for the TTL)
	unindexed map[string]struct{} // intended-but-skipped files : always scanned
	dirty     map[string]int64    // abs path → UnixNano of last write : always scanned
	usedAt    time.Time           // last query (for LRU eviction)
}

type tindexManager struct {
	mu       sync.Mutex
	cacheDir string
	byRoot   map[string]*tindex
}

var tindexes = &tindexManager{byRoot: map[string]*tindex{}}

// cacheDirOnce resolves (and creates) the index cache dir once. It lives under
// the OS temp dir — never inside a workspace, so an agent's own grep can neither
// see nor confine to the .idx files.
func (m *tindexManager) dir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cacheDir == "" {
		d := filepath.Join(os.TempDir(), "digitorn-tindex")
		_ = os.MkdirAll(d, 0o700)
		m.cacheDir = d
	}
	return m.cacheDir
}

// get returns the tindex for root, creating it on first use and evicting the
// least-recently-used one if the hot set is full.
func (m *tindexManager) get(root string, maxBytes int64) *tindex {
	dir := m.dir()
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.byRoot[root]; ok {
		return t
	}
	if len(m.byRoot) >= tindexMaxRoots {
		m.evictLRULocked()
	}
	sum := sha256.Sum256([]byte(root))
	t := &tindex{
		root:     root,
		file:     filepath.Join(dir, hex.EncodeToString(sum[:12])), // build appends ".<gen>.idx"
		maxBytes: maxBytes,
	}
	m.byRoot[root] = t
	return t
}

// markDirty flags abs as changed in every index whose root contains it. Called
// from write/edit so a freshly written file is always rescanned, never served
// from a stale trigram entry. Cheap : a handful of hot roots, map insert each.
func (m *tindexManager) markDirty(abs string) {
	now := time.Now().UnixNano()
	m.mu.Lock()
	roots := make([]*tindex, 0, len(m.byRoot))
	for _, t := range m.byRoot {
		roots = append(roots, t)
	}
	m.mu.Unlock()
	for _, t := range roots {
		if underRoot(t.root, abs) {
			t.markDirty(abs, now)
		}
	}
}

// underRoot reports whether p lies within (or equals) root.
func underRoot(root, p string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (m *tindexManager) evictLRULocked() {
	var oldestKey string
	var oldest time.Time
	for k, t := range m.byRoot {
		t.mu.Lock()
		u := t.usedAt
		t.mu.Unlock()
		if oldestKey == "" || u.Before(oldest) {
			oldestKey, oldest = k, u
		}
	}
	if oldestKey == "" {
		return
	}
	victim := m.byRoot[oldestKey]
	delete(m.byRoot, oldestKey)
	victim.mu.Lock()
	if victim.ix != nil {
		_ = victim.ix.Close()
		victim.ix = nil
	}
	if victim.openPath != "" {
		_ = os.Remove(victim.openPath) // don't leave the .idx behind in temp
		victim.openPath = ""
	}
	victim.state = tsIdle
	victim.mu.Unlock()
}

// markDirty records that abs was written/edited, so it is scanned fresh on the
// next query regardless of what the (now stale) index says about it.
func (t *tindex) markDirty(abs string, nowNano int64) {
	t.mu.Lock()
	if t.dirty == nil {
		t.dirty = map[string]int64{}
	}
	t.dirty[abs] = nowNano
	t.mu.Unlock()
}

// maybeBuild kicks an async (re)build when there is no index yet or the current
// one is stale, unless one is already running. The old index keeps serving until
// the rebuild publishes.
func (t *tindex) maybeBuild() {
	t.mu.Lock()
	if t.building {
		t.mu.Unlock()
		return
	}
	if t.state == tsReady && !t.staleLocked() {
		t.mu.Unlock()
		return
	}
	t.building = true
	t.mu.Unlock()
	go t.build()
}

func (t *tindex) staleLocked() bool {
	if time.Since(t.builtAt) > tindexTTL {
		return true
	}
	threshold := tindexDirtyFloor
	if p := t.nfiles / tindexDirtyPercent; p > threshold {
		threshold = p
	}
	return len(t.dirty) > threshold
}

// build walks the root subtree, indexes every regular text file under the size
// cap, atomically swaps in the new index, and computes the always-scan sets.
// It NEVER takes down the daemon : any codesearch panic is recovered and leaves
// the previous index (if any) in place so queries keep working / fall back.
func (t *tindex) build() {
	t.mu.Lock()
	t.gen++
	gen := t.gen
	t.mu.Unlock()
	startedAt := time.Now().UnixNano()

	ok, newIx, newPath, nfiles, unindexed := t.buildLocked(gen)

	t.mu.Lock()
	t.building = false
	if !ok {
		// Build failed : keep any existing index, retry on a later trigger.
		t.mu.Unlock()
		return
	}
	oldIx, oldPath := t.ix, t.openPath
	t.ix = newIx
	t.openPath = newPath
	t.nfiles = nfiles
	t.unindexed = unindexed
	t.builtAt = time.Now()
	t.state = tsReady
	// Drop dirty entries the build already saw (marked before it started) ; keep
	// anything re-dirtied during the build so a concurrent write is never lost.
	for p, ts := range t.dirty {
		if ts < startedAt {
			delete(t.dirty, p)
		}
	}
	t.mu.Unlock()

	// Release the superseded index OUTSIDE the lock : its file is a distinct
	// generation from the one just opened, so closing + deleting it can never race
	// the live index or block a query.
	if oldIx != nil {
		_ = oldIx.Close()
	}
	if oldPath != "" && oldPath != newPath {
		_ = os.Remove(oldPath)
	}
}

// buildLocked does the heavy lifting without holding t.mu (so queries against the
// old index never block on a rebuild). Each build writes a UNIQUE generation file
// so publishing never has to rename over the still-mmapped previous index (which
// Windows forbids). Returns ok=false on any failure.
func (t *tindex) buildLocked(gen int) (ok bool, ix *csindex.Index, path string, nfiles int, unindexed map[string]struct{}) {
	defer func() {
		if r := recover(); r != nil {
			ok, ix, path, nfiles, unindexed = false, nil, "", 0, nil
		}
	}()

	tmp := fmt.Sprintf("%s.%d.building", t.file, gen)
	final := fmt.Sprintf("%s.%d.idx", t.file, gen)
	_ = os.Remove(tmp)

	// attempted = the text files we actually fed to the indexer (binary, empty,
	// oversized, and unreadable files are dropped here — the scanner never matches
	// them either, so excluding them is correct, not lossy).
	var attempted []string
	w := csindex.Create(tmp)
	for _, p := range t.collectIndexable() {
		r, closeFn, good := openForIndex(p, t.maxBytes)
		if !good {
			continue
		}
		w.Add(p, r)
		closeFn()
		attempted = append(attempted, p)
	}
	w.Flush()
	if err := w.Close(); err != nil { // release the handle so the rename can proceed
		_ = os.Remove(tmp)
		return false, nil, "", 0, nil
	}

	// Atomic publish : rename the fully-written temp to its final generation path
	// so a reader never observes a half-written (panic-inducing) index.
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return false, nil, "", 0, nil
	}

	ix = csindex.Open(final)
	indexed := make(map[string]struct{}, ix.NumFiles())
	for id := 0; id < ix.NumFiles(); id++ {
		indexed[ix.Name(uint32(id))] = struct{}{}
	}
	// unindexed = text files the indexer silently rejected for ITS own reasons
	// (invalid UTF-8, over-long lines, too many trigrams). The scanner WOULD match
	// these, so they must always be scanned to keep results complete.
	unindexed = map[string]struct{}{}
	for _, p := range attempted {
		if _, in := indexed[p]; !in {
			unindexed[p] = struct{}{}
		}
	}
	return true, ix, final, len(attempted), unindexed
}

// collectIndexable walks the root subtree exactly like the scanner's full walk
// (same skip-dirs, regular-files-only) so the index covers precisely the set the
// scanner would visit. Per-query confinement/include is applied later, so this
// intentionally indexes the whole subtree as a safe superset.
func (t *tindex) collectIndexable() []string {
	var out []string
	_ = filepath.WalkDir(t.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, skip := skipDirs[d.Name()]; skip && p != t.root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// candidates resolves pattern to a trigram query and returns the files that could
// match (index hits ∪ unindexed ∪ dirty). usable is false when there is no index
// or the pattern yields no trigram narrowing (QAll) — the caller then full-scans.
// Held entirely under mu so a concurrent rebuild cannot unmap ix mid-query ; a
// corrupt index surfacing here is recovered, dropped, and reported unusable.
func (t *tindex) candidates(pattern string) (paths []string, usable bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usedAt = time.Now()
	if t.state != tsReady || t.ix == nil {
		return nil, false
	}
	defer func() {
		if r := recover(); r != nil {
			if t.ix != nil {
				_ = t.ix.Close()
			}
			t.ix = nil
			t.state = tsIdle
			paths, usable = nil, false
		}
	}()

	re, err := csregexp.Compile(pattern)
	if err != nil {
		return nil, false // pattern codesearch can't analyze : fall back to scan
	}
	q := csindex.RegexpQuery(re.Syntax)
	if q.Op == csindex.QAll {
		return nil, false // no useful trigrams (too short / too general) : full scan
	}

	ids := t.ix.PostingQuery(q)
	set := make(map[string]struct{}, len(ids)+len(t.unindexed)+len(t.dirty))
	for _, id := range ids {
		set[t.ix.Name(id)] = struct{}{}
	}
	for p := range t.unindexed {
		set[p] = struct{}{}
	}
	for p := range t.dirty {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out, true
}

// openForIndex opens path for indexing, skipping non-regular, empty, oversized,
// and binary files — exactly the files the scanner would also never match. The
// returned reader yields the already-read binary-detection peek followed by the
// rest of the file, so the content is read only once.
func openForIndex(path string, maxBytes int64) (io.Reader, func(), bool) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, nil, false
	}
	limit := maxBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	if sz := st.Size(); sz == 0 || sz > limit {
		return nil, nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	peek := make([]byte, 8192)
	n, _ := io.ReadFull(f, peek)
	peek = peek[:n]
	if isBinary(peek) {
		_ = f.Close()
		return nil, nil, false
	}
	return io.MultiReader(bytes.NewReader(peek), f), func() { _ = f.Close() }, true
}
