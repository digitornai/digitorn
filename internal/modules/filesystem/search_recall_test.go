package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// search_recall_test.go : the POWER proof. Speed is worthless if the agent misses
// a match that exists. These tests pit our trigram-indexed grep against an
// INDEPENDENT oracle — a dead-simple "reference grep" : read every eligible file,
// run Go's stdlib regexp over the raw bytes, record one (file:line) per matching
// line. No index, no trigrams, no fast paths. If the indexed grep returns the
// EXACT same set as the oracle across a battery of powerful queries AND a random
// fuzz, then recall is 100% (never misses) and precision is 100% (no phantom hit).

// oracleSearch is the reference implementation : the obviously-correct ground
// truth. It deliberately shares NONE of the production matcher / index code — it
// only mirrors the same file-eligibility policy (skip VCS dirs, skip binary, size
// cap) so the comparison isolates the matching+narrowing, not the file selection.
func oracleSearch(t *testing.T, root, base, pattern string, multiline bool) map[string]bool {
	t.Helper()
	// Compile per compilePattern's documented contract, but ALWAYS via regexp
	// (even literals, via QuoteMeta) — independent of production's bytes.Index path.
	expr := pattern
	if !multiline && regexp.QuoteMeta(pattern) == pattern {
		expr = "(?m)" + regexp.QuoteMeta(pattern)
	} else if multiline {
		expr = "(?sm)" + pattern
	} else {
		expr = "(?m)" + pattern
	}
	re := regexp.MustCompile(expr)

	out := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, skip := skipDirs[d.Name()]; skip && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, e := d.Info()
		if e != nil || info.Size() == 0 || info.Size() > 10<<20 {
			return nil
		}
		buf, e := os.ReadFile(p)
		if e != nil {
			return nil
		}
		// Independent binary check : a NUL in the first 8 KiB → skip (matches the
		// scanner's contract without calling its code).
		head := buf
		if len(head) > 8192 {
			head = head[:8192]
		}
		for _, b := range head {
			if b == 0 {
				return nil
			}
		}
		rel := relForCompare(base, p)
		for _, loc := range re.FindAllIndex(buf, -1) {
			line := 1 + strings.Count(string(buf[:loc[0]]), "\n")
			out[fmt.Sprintf("%s:%d", rel, line)] = true
		}
		return nil
	})
	return out
}

// relForCompare derives the same workspace-relative, slash-normalised path the
// production grep reports, so the two result sets are directly comparable.
func relForCompare(base, abs string) string {
	real := abs
	if r, e := filepath.EvalSymlinks(abs); e == nil {
		real = r
	}
	rel, err := filepath.Rel(base, real)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// oursSearch runs the production indexed grep and returns the same (file:line) set.
func oursSearch(t *testing.T, m *Module, pattern string, multiline bool) map[string]bool {
	t.Helper()
	r, err := m.grep(context.Background(), mustJSON(map[string]any{
		"pattern": pattern, "multiline": multiline, "max_results": 1000000,
	}))
	if err != nil || !r.Success {
		t.Fatalf("ours grep %q failed: %v (%v)", pattern, err, r.Error)
	}
	out := map[string]bool{}
	for _, mm := range r.Data.(map[string]any)["matches"].([]grepMatch) {
		out[fmt.Sprintf("%s:%d", mm.File, mm.LineNum)] = true
	}
	return out
}

func diffSets(a, b map[string]bool) (onlyA, onlyB []string) {
	for k := range a {
		if !b[k] {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if !a[k] {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return
}

// forceIndexReady builds the per-root index synchronously so the indexed path is
// actually exercised (not the cold-start full-scan fallback).
func forceIndexReady(t *testing.T, m *Module) {
	t.Helper()
	root, err := m.resolveCtx(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	ti := tindexes.get(root, m.cfg.MaxFileBytes)
	ti.build()
	if ti.state != tsReady {
		t.Fatal("index not ready")
	}
	t.Cleanup(func() { ti.closeForTest() })
}

// TestSearchRecall_PowerfulQueries is the headline proof : a curated battery of
// hard searches — regex, anchors, alternation, unicode, emoji, multiline, plus the
// nasty cases (a file too long-lined to index, a brand-new dirty file, files in
// skipped dirs, a binary file) — must return EXACTLY what the oracle returns.
func TestSearchRecall_PowerfulQueries(t *testing.T) {
	m, ws := setupFS(t)

	// --- a realistic + adversarial corpus -----------------------------------
	for i := 0; i < 60; i++ {
		writeFile(t, ws, fmt.Sprintf("pkg%02d/f%03d.go", i%6, i),
			fmt.Sprintf("package pkg%02d\nfunc Handler%d() error {\n\treturn computeValue(%d)\n}\n", i%6, i, i))
	}
	writeFile(t, ws, "alpha.go", "package a\nfunc Foo() {}\nfunc Bar2() {}\n// TODO: refactor\n")
	writeFile(t, ws, "unicode.txt", "greeting: café\nlang: 日本語\nemoji: 😀 done\nname: naïve coöperate\n")
	writeFile(t, ws, "multi.txt", "start\nalpha marker\nmiddle\nbeta marker\nend\n")
	// Over-long line (>2000 chars) → codesearch refuses to index it → must be found
	// via the always-scanned "unindexed" set.
	writeFile(t, ws, "longline.txt", strings.Repeat("filler ", 400)+"NEEDLE_LONGLINE"+strings.Repeat(" x", 400)+"\n")
	// Files in a skipped dir → neither tool should find them.
	writeFile(t, ws, "node_modules/dep.go", "func Foo() {} // NEEDLE_SKIPPED\n")
	// A binary file (NUL) → neither tool should find it.
	writeFile(t, ws, "blob.bin", "NEEDLE_BINARY\x00\x01\x02more")

	forceIndexReady(t, m)

	// A file created AFTER the index was built. Its write marks it dirty, so it
	// must still be found (freshness). Mirror production : write through the module.
	wr, err := m.write(context.Background(), mustJSON(map[string]any{
		"path": "fresh.go", "content": "package fresh\nvar X = NEEDLE_DIRTY\n",
	}))
	if err != nil || !wr.Success {
		t.Fatalf("write fresh.go: %v (%v)", err, wr.Error)
	}

	base := ws
	if rb, e := filepath.EvalSymlinks(ws); e == nil {
		base = rb
	}

	cases := []struct {
		name      string
		pattern   string
		multiline bool
	}{
		{"plain-literal", "computeValue", false},
		{"regex-func-decl", `func \w+\(`, false},
		{"anchored-package", `^package pkg03`, false},
		{"alternation", `Foo|Bar2`, false},
		{"char-class-digit", `Handler\d+`, false},
		{"dollar-anchor", `\}$`, false},
		{"literal-with-meta", "computeValue(7)", false},
		{"unicode-cafe", "café", false},
		{"unicode-cjk", "日本語", false},
		{"emoji", "😀", false},
		{"unicode-diacritic", "coöperate", false},
		{"multiline-span", "alpha marker.*beta marker", true},
		{"unindexed-longline", "NEEDLE_LONGLINE", false},
		{"dirty-fresh-file", "NEEDLE_DIRTY", false},
		{"in-skipped-dir", "NEEDLE_SKIPPED", false},   // expect 0 both
		{"in-binary-file", "NEEDLE_BINARY", false},    // expect 0 both
		{"absent-token", "TOTALLY_ABSENT_ZZZ", false}, // expect 0 both
		{"comment-todo", "TODO: refactor", false},
	}

	t.Logf("%-22s %8s %8s   %s", "QUERY", "ORACLE", "OURS", "VERDICT")
	t.Logf("%s", strings.Repeat("-", 60))
	for _, c := range cases {
		oracle := oracleSearch(t, ws, base, c.pattern, c.multiline)
		ours := oursSearch(t, m, c.pattern, c.multiline)
		onlyOracle, onlyOurs := diffSets(oracle, ours)
		verdict := "✓ identical"
		if len(onlyOracle) > 0 || len(onlyOurs) > 0 {
			verdict = "✗ MISMATCH"
		}
		t.Logf("%-22s %8d %8d   %s", c.name, len(oracle), len(ours), verdict)
		if len(onlyOracle) > 0 {
			t.Errorf("query %q : MISSED %d real matches the oracle found: %v", c.name, len(onlyOracle), onlyOracle)
		}
		if len(onlyOurs) > 0 {
			t.Errorf("query %q : reported %d phantom matches not in oracle: %v", c.name, len(onlyOurs), onlyOurs)
		}
	}
}

// TestSearchRecall_Fuzz hammers recall with random corpora and random query tokens
// (some present, some absent). Every single query's result set must equal the
// oracle's — across hundreds of randomized trials. This is the anti-"I only tested
// the cases I thought of" guard.
func TestSearchRecall_Fuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fuzz in -short")
	}
	// Deterministic PRNG (no time/rand seed needed) so failures reproduce.
	rng := uint64(0x9e3779b97f4a7c15)
	next := func() uint64 { rng ^= rng << 13; rng ^= rng >> 7; rng ^= rng << 17; return rng }
	tok := func(n int) string {
		const al = "abcdefghijklmnopqrstuvwxyz0123456789"
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(al[next()%uint64(len(al))])
		}
		return b.String()
	}

	m, ws := setupFS(t)
	planted := make([]string, 0, 40)
	for i := 0; i < 250; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, "package p\n")
		lines := 5 + int(next()%30)
		for l := 0; l < lines; l++ {
			// ~8% of lines carry a 6-char token we remember as "definitely present".
			if next()%12 == 0 {
				tk := "TK" + tok(6)
				planted = append(planted, tk)
				fmt.Fprintf(&sb, "x := %s_call(%d)\n", tk, l)
			} else {
				fmt.Fprintf(&sb, "y%d := compute(%d)\n", l, l)
			}
		}
		writeFile(t, ws, fmt.Sprintf("d%02d/f%04d.go", i%16, i), sb.String())
	}

	forceIndexReady(t, m)
	base := ws
	if rb, e := filepath.EvalSymlinks(ws); e == nil {
		base = rb
	}

	queries := make([]string, 0, 200)
	// Half from planted tokens (must be found), half random (mostly absent).
	for i := 0; i < 100 && i < len(planted); i++ {
		queries = append(queries, planted[int(next()%uint64(len(planted)))])
	}
	for i := 0; i < 100; i++ {
		queries = append(queries, "TK"+tok(6))
	}

	mismatches := 0
	for _, q := range queries {
		oracle := oracleSearch(t, ws, base, q, false)
		ours := oursSearch(t, m, q, false)
		onlyOracle, onlyOurs := diffSets(oracle, ours)
		if len(onlyOracle) > 0 || len(onlyOurs) > 0 {
			mismatches++
			t.Errorf("fuzz query %q : missed=%v phantom=%v", q, onlyOracle, onlyOurs)
		}
	}
	t.Logf("fuzz: %d randomized queries, %d mismatches (0 = perfect recall+precision)", len(queries), mismatches)
}
