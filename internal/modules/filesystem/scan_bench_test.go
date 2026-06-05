package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildCorpus writes a synthetic source tree: files files of ~linesPerFile
// lines, a fraction containing the needle. Returns the root.
func buildCorpus(b *testing.B, files, linesPerFile int) string {
	b.Helper()
	root := b.TempDir()
	for i := 0; i < files; i++ {
		var sb []byte
		for l := 0; l < linesPerFile; l++ {
			if l == linesPerFile/2 && i%7 == 0 {
				sb = append(sb, []byte("    return errNeedleHere // hit\n")...)
			} else {
				sb = append(sb, []byte(fmt.Sprintf("\tx%d := compute(%d, %d)\n", l, i, l))...)
			}
		}
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", i%32))
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.go", i)), sb, 0o644)
	}
	return root
}

// BenchmarkGrep_Literal measures the parallel literal fast path over a sizeable
// tree — the search hot path an agent hammers. Reports B/op + allocs/op.
func BenchmarkGrep_Literal(b *testing.B) {
	root := buildCorpus(b, 2000, 200) // ~2000 files, ~400k lines
	re, lit, err := compilePattern("errNeedleHere", false)
	if err != nil {
		b.Fatal(err)
	}
	relFn := func(abs string) (string, bool) {
		r, e := filepath.Rel(root, abs)
		return filepath.ToSlash(r), e == nil
	}
	req := grepRequest{
		root: root, re: re, literal: lit, mode: grepContent,
		maxResults: 100000, rel: relFn,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := runGrep(context.Background(), req, walkEnum(req))
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			b.Logf("scanned=%d matches=%d", res.Scanned, len(res.Matches))
		}
	}
}

// BenchmarkGrep_Regex measures the regexp path (no literal fast path) over the
// same tree, to size the regex overhead.
func BenchmarkGrep_Regex(b *testing.B) {
	root := buildCorpus(b, 2000, 200)
	re, lit, err := compilePattern(`errNeedle\w+`, false)
	if err != nil {
		b.Fatal(err)
	}
	relFn := func(abs string) (string, bool) {
		r, e := filepath.Rel(root, abs)
		return filepath.ToSlash(r), e == nil
	}
	req := grepRequest{
		root: root, re: re, literal: lit, mode: grepContent,
		maxResults: 100000, rel: relFn,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runGrep(context.Background(), req, walkEnum(req)); err != nil {
			b.Fatal(err)
		}
	}
}
