//go:build treesitter

package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/codeast"
)

// graphStats summarises a built graph : how many files carry definitions,
// how many symbols (nodes), caller-edges and import-edges it holds.
func graphStats(g *codeGraph) (files, syms, callerEdges, importEdges int) {
	files = len(g.byFile)
	for _, ds := range g.byFile {
		syms += len(ds)
	}
	for _, cs := range g.callers {
		callerEdges += len(cs)
	}
	for _, is := range g.imports {
		importEdges += len(is)
	}
	return
}

// dirStats walks a tree and totals the recognised-source files + bytes the
// graph builder would actually parse (Go/Py/JS/TS, under maxBytes).
func dirStats(root string, maxBytes int64) (files int, bytes int64) {
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
		info, e := d.Info()
		if e != nil || (maxBytes > 0 && info.Size() > maxBytes) {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return
}

func buildGoCorpus(tb testing.TB, nFiles, funcsPerFile int) string {
	dir := tb.TempDir()
	for f := 0; f < nFiles; f++ {
		var sb strings.Builder
		sb.WriteString("package pkg\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n")
		for fn := 0; fn < funcsPerFile; fn++ {
			sb.WriteString("func Handler")
			sb.WriteString(itoa(f))
			sb.WriteString("_")
			sb.WriteString(itoa(fn))
			sb.WriteString("(w int, r string) error {\n")
			sb.WriteString("\tif strings.TrimSpace(r) == \"\" {\n\t\treturn nil\n\t}\n")
			sb.WriteString("\thelper")
			sb.WriteString(itoa(fn))
			sb.WriteString("(w)\n\tfmt.Println(r)\n\treturn nil\n}\n\n")
		}
		sb.WriteString("func helper0(n int) {}\n")
		name := filepath.Join(dir, "f"+itoa(f)+".go")
		if err := os.WriteFile(name, []byte(sb.String()), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func BenchmarkCodeGraph_Build_Corpus(b *testing.B) {
	root := buildGoCorpus(b, 600, 8)
	files, bytes := dirStats(root, 1<<20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildGraph(root, 1<<20)
	}
	b.StopTimer()
	g := buildGraph(root, 1<<20)
	gf, sy, ce, ie := graphStats(g)
	secs := b.Elapsed().Seconds() / float64(b.N)
	b.Logf("corpus: %d src files (%.1f MB) -> %d def-files, %d symbols, %d call-edges, %d import-edges | %.0f files/s, %.1f MB/s",
		files, float64(bytes)/1e6, gf, sy, ce, ie, float64(files)/secs, float64(bytes)/1e6/secs)
}

// BenchmarkCodeGraph_Build_RealRepo builds the graph over the daemon's own
// internal/ tree — a real, mixed-size codebase — to report the actual
// injection speed (files/s, MB/s) relative to codebase size.
func BenchmarkCodeGraph_Build_RealRepo(b *testing.B) {
	root, err := filepath.Abs("../..") // internal/
	if err != nil {
		b.Fatal(err)
	}
	if _, e := os.Stat(root); e != nil {
		b.Skipf("real repo tree not found: %v", e)
	}
	files, bytes := dirStats(root, 1<<20)
	if files == 0 {
		b.Skip("no source files under real repo tree")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildGraph(root, 1<<20)
	}
	b.StopTimer()
	g := buildGraph(root, 1<<20)
	gf, sy, ce, ie := graphStats(g)
	secs := b.Elapsed().Seconds() / float64(b.N)
	b.Logf("real repo %s: %d src files (%.1f MB) -> %d def-files, %d symbols, %d call-edges, %d import-edges | %.0f files/s, %.1f MB/s, %.0f symbols/s",
		root, files, float64(bytes)/1e6, gf, sy, ce, ie, float64(files)/secs, float64(bytes)/1e6/secs, float64(sy)/secs)
}

// BenchmarkAstChunks_Go isolates single-file AST parsing throughput.
func BenchmarkAstChunks_Go(b *testing.B) {
	src := []byte(strings.Repeat(
		"func Handler(w int, r string) error {\n\tif r == \"\" { return nil }\n\thelper(w)\n\tfmt.Println(r)\n\treturn nil\n}\n",
		40) + "func helper(n int) {}\n")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = astChunks("h.go", src)
	}
}
