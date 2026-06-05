package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	csindex "github.com/mbathepaul/digitorn/internal/csearch/index"
)

// buildIndexNow constructs an isolated tindex over root and builds it
// synchronously (no async goroutine), so tests are deterministic.
func buildIndexNow(t *testing.T, root string) *tindex {
	t.Helper()
	ti := &tindex{
		root:     root,
		file:     filepath.Join(t.TempDir(), "test"),
		maxBytes: 10 << 20,
	}
	ti.build()
	if ti.state != tsReady {
		t.Fatalf("index did not reach ready state")
	}
	t.Cleanup(func() { ti.closeForTest() }) // release the mmap so TempDir cleanup can unlink it
	return ti
}

// closeForTest releases the mmapped index so the OS can delete the backing file
// (Windows keeps a mapped file locked until every view is unmapped).
func (t *tindex) closeForTest() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ix != nil {
		_ = t.ix.Close()
		t.ix = nil
	}
	t.state = tsIdle
}

func reqFor(root, pattern string, multiline bool) grepRequest {
	re, lit, err := compilePattern(pattern, multiline)
	if err != nil {
		panic(err)
	}
	return grepRequest{
		root: root, re: re, literal: lit, mode: grepContent, maxResults: 100000,
		rel: func(abs string) (string, bool) {
			r, e := filepath.Rel(root, abs)
			return filepath.ToSlash(r), e == nil
		},
	}
}

func matchSet(ms []grepMatch) map[string]bool {
	s := make(map[string]bool, len(ms))
	for _, m := range ms {
		s[fmt.Sprintf("%s:%d", m.File, m.LineNum)] = true
	}
	return s
}

// TestTindex_IndexedMatchesFullScan is the core correctness guarantee : for every
// pattern that the index narrows, scanning only the candidates yields EXACTLY the
// matches a full tree scan would. A trigram index that drops a real match is worse
// than useless, so this parity is non-negotiable.
func TestTindex_IndexedMatchesFullScan(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < 120; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, "package pkg%02d\n", i%9)
		for l := 0; l < 40; l++ {
			if l == 20 && i%5 == 0 {
				sb.WriteString("\treturn errNeedleHere // sentinel hit\n")
			} else {
				fmt.Fprintf(&sb, "\tvalue%d := compute(%d, %d)\n", l, i, l)
			}
		}
		writeFile(t, ws, fmt.Sprintf("pkg%02d/f%03d.go", i%9, i), sb.String())
	}

	ti := buildIndexNow(t, ws)
	ctx := context.Background()

	for _, pat := range []string{"errNeedleHere", "compute", "package pkg03", `errNeedle\w+`} {
		req := reqFor(ws, pat, false)
		full, err := runGrep(ctx, req, walkEnum(req))
		if err != nil {
			t.Fatalf("full scan %q: %v", pat, err)
		}
		cand, usable := ti.candidates(pat)
		if !usable {
			// QAll (no trigram narrowing) → production full-scans ; nothing to compare.
			continue
		}
		idx, err := runGrep(ctx, req, listEnum(req, cand))
		if err != nil {
			t.Fatalf("indexed scan %q: %v", pat, err)
		}
		fs, is := matchSet(full.Matches), matchSet(idx.Matches)
		if len(fs) != len(is) {
			t.Fatalf("pattern %q : indexed found %d matches, full scan found %d", pat, len(is), len(fs))
		}
		for k := range fs {
			if !is[k] {
				t.Errorf("pattern %q : indexed scan MISSED %s (candidates=%d)", pat, k, len(cand))
			}
		}
	}
}

// TestTindex_DirtyFileFoundAfterBuild proves freshness : a file created/written
// after the index was built is still found, because writes mark it dirty and the
// dirty set is always scanned alongside the index candidates.
func TestTindex_DirtyFileFoundAfterBuild(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "existing.go", "package a\nfunc untouched() {}\n")

	ti := buildIndexNow(t, ws)

	// A brand-new file the index has never seen.
	newAbs := filepath.Join(ws, "fresh.go")
	if err := os.WriteFile(newAbs, []byte("package a\n// SENTINELDIRTY marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ti.markDirty(newAbs, time.Now().UnixNano())

	cand, usable := ti.candidates("SENTINELDIRTY")
	if !usable {
		t.Fatal("SENTINELDIRTY should yield trigram narrowing")
	}
	req := reqFor(ws, "SENTINELDIRTY", false)
	res, err := runGrep(context.Background(), req, listEnum(req, cand))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || filepath.Base(res.Matches[0].File) != "fresh.go" {
		t.Fatalf("dirty file not found via index path: %+v", res.Matches)
	}
}

// TestTindex_UnindexedFileAlwaysScanned proves completeness against codesearch's
// own skips : a file with a >2000-char line is rejected by the indexer, lands in
// the always-scan unindexed set, and is therefore still matched.
func TestTindex_UnindexedFileAlwaysScanned(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "normal.go", "package a\nfunc f() {}\n")
	// One enormous line (> maxLineLen=2000) → indexer skips it, scanner must not.
	huge := strings.Repeat("a", 3000) + "SENTINELHUGE" + strings.Repeat("b", 3000) + "\n"
	writeFile(t, ws, "huge.go", huge)

	ti := buildIndexNow(t, ws)

	hugeAbs := filepath.Join(ws, "huge.go")
	if _, in := ti.unindexed[hugeAbs]; !in {
		t.Fatalf("over-long-line file should be in the always-scan unindexed set, got %v", ti.unindexed)
	}
	cand, usable := ti.candidates("SENTINELHUGE")
	if !usable {
		t.Fatal("SENTINELHUGE should yield trigram narrowing")
	}
	req := reqFor(ws, "SENTINELHUGE", false)
	res, err := runGrep(context.Background(), req, listEnum(req, cand))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || filepath.Base(res.Matches[0].File) != "huge.go" {
		t.Fatalf("unindexed file not found (completeness broken): %+v", res.Matches)
	}
}

// TestTindex_ShortPatternFallsBack : a pattern with no trigram (< 3 effective
// chars) yields QAll, so candidates reports unusable and grep full-scans.
func TestTindex_ShortPatternFallsBack(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.go", "ab cd ef\n")
	ti := buildIndexNow(t, ws)
	if _, usable := ti.candidates("ab"); usable {
		t.Error("2-char pattern must not be usable (no trigram narrowing)")
	}
}

// TestTindex_GrepUsesIndexAndStaysCorrect drives the real m.grep path with the
// per-root index forced ready, proving the production wiring returns the same
// result the full-scan path does (skip-dirs honoured, binaries skipped).
func TestTindex_GrepUsesIndexAndStaysCorrect(t *testing.T) {
	m, _ := setupTree(t)
	ctx := context.Background()
	root, err := m.resolveCtx(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	ti := tindexes.get(root, m.cfg.MaxFileBytes)
	ti.build()
	if ti.state != tsReady {
		t.Fatal("index not ready")
	}
	t.Cleanup(func() { ti.closeForTest() })
	r, err := m.grep(ctx, mustJSON(map[string]any{"pattern": "Foo"}))
	if err != nil || !r.Success {
		t.Fatalf("grep: %v (%v)", err, r.Error)
	}
	matches := r.Data.(map[string]any)["matches"].([]grepMatch)
	if len(matches) != 3 {
		t.Fatalf("indexed grep want 3 matches, got %d: %+v", len(matches), matches)
	}
	for _, mm := range matches {
		if base := filepath.Base(mm.File); base == "dep.go" || base == "bin.dat" {
			t.Errorf("indexed grep returned a file it should skip: %s", base)
		}
	}
}

// TestTindex_OpenCorruptRecovers locks down the reliability contract : a corrupt
// index makes the vendored library panic (NOT os.Exit, which would kill the whole
// daemon), so the panic is recoverable and degrades to a full scan.
func TestTindex_OpenCorruptRecovers(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "corrupt.idx")
	if err := os.WriteFile(bad, []byte("this is not a real csearch trigram index at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered := func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = true
			}
		}()
		_ = csindex.Open(bad)
		return false
	}()
	if !recovered {
		t.Fatal("opening a corrupt index must panic (recoverable), never os.Exit")
	}
}

// BenchmarkGrep_IndexedRare is the realistic agent case : searching for a SPECIFIC
// symbol that lives in only a few files. The trigram index narrows to that handful,
// so the query is dominated by the (sub-ms) posting lookup, not a corpus rescan.
func BenchmarkGrep_IndexedRare(b *testing.B) {
	root := buildCorpus(b, 2000, 200)
	// Plant a unique token in just 3 of the 2000 files.
	for _, n := range []string{"pkg00/rare_a.go", "pkg11/rare_b.go", "pkg29/rare_c.go"} {
		p := filepath.Join(root, filepath.FromSlash(n))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("package x\nconst Z = \"ZQXRARETOKEN7\"\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	ti := &tindex{root: root, file: filepath.Join(b.TempDir(), "rare"), maxBytes: 10 << 20}
	ti.build()
	if ti.state != tsReady {
		b.Fatal("index not ready")
	}
	b.Cleanup(func() { ti.closeForTest() })
	req := reqFor(root, "ZQXRARETOKEN7", false)
	ctx := context.Background()
	if c, usable := ti.candidates("ZQXRARETOKEN7"); !usable {
		b.Fatal("expected trigram narrowing")
	} else {
		b.Logf("candidates=%d of 2003 files", len(c))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cand, _ := ti.candidates("ZQXRARETOKEN7")
		res, err := runGrep(ctx, req, listEnum(req, cand))
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			b.Logf("scanned=%d matches=%d", res.Scanned, len(res.Matches))
		}
	}
}

// BenchmarkTindex_LookupOnly isolates the pure index lookup (no confirm-scan) so
// the trigram-resolution cost is visible on its own.
func BenchmarkTindex_LookupOnly(b *testing.B) {
	root := buildCorpus(b, 2000, 200)
	ti := &tindex{root: root, file: filepath.Join(b.TempDir(), "lookup"), maxBytes: 10 << 20}
	ti.build()
	if ti.state != tsReady {
		b.Fatal("index not ready")
	}
	b.Cleanup(func() { ti.closeForTest() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, usable := ti.candidates("errNeedleHere"); !usable {
			b.Fatal("expected usable")
		}
	}
}

// BenchmarkGrep_Indexed measures the trigram fast path : resolve the pattern to a
// candidate set, then scan only those. Compare against BenchmarkGrep_Literal which
// scans the whole tree. The index build is excluded from the timer (one-time cost).
func BenchmarkGrep_Indexed(b *testing.B) {
	root := buildCorpus(b, 2000, 200) // same tree as BenchmarkGrep_Literal
	ti := &tindex{root: root, file: filepath.Join(b.TempDir(), "bench"), maxBytes: 10 << 20}
	ti.build()
	if ti.state != tsReady {
		b.Fatal("index not ready")
	}
	b.Cleanup(func() { ti.closeForTest() })
	req := reqFor(root, "errNeedleHere", false)
	ctx := context.Background()
	if c, usable := ti.candidates("errNeedleHere"); !usable {
		b.Fatal("expected trigram narrowing")
	} else {
		b.Logf("candidates=%d of 2000 files", len(c))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cand, _ := ti.candidates("errNeedleHere")
		res, err := runGrep(ctx, req, listEnum(req, cand))
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			b.Logf("scanned=%d matches=%d", res.Scanned, len(res.Matches))
		}
	}
}
