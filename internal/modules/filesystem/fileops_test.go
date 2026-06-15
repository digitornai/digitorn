package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- read : line-based + numbers + kind detection -------------------------

func TestRead_LineNumbersAndSlice(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.go", "one\ntwo\nthree\nfour\nfive\n")

	// Full read : numbered, all lines present.
	r, err := m.read(context.Background(), mustJSON(map[string]any{"path": "f.go"}))
	if err != nil || !r.Success {
		t.Fatalf("read: %v (%v)", err, r.Error)
	}
	body := r.Data.(string)
	for i, want := range []string{"1\tone", "2\ttwo", "5\tfive"} {
		if !strings.Contains(body, want) {
			t.Errorf("line %d: output missing %q\n%s", i, want, body)
		}
	}

	// Slice : offset=2, limit=2 → lines 2-3 only, with a continuation hint.
	r2, _ := m.read(context.Background(), mustJSON(map[string]any{"path": "f.go", "offset": 2, "limit": 2}))
	b2 := r2.Data.(string)
	if !strings.Contains(b2, "2\ttwo") || !strings.Contains(b2, "3\tthree") {
		t.Errorf("slice missing lines 2-3: %s", b2)
	}
	if strings.Contains(b2, "four") {
		t.Errorf("slice leaked line 4: %s", b2)
	}
	if !strings.Contains(b2, "offset=4") {
		t.Errorf("slice should hint the next offset: %s", b2)
	}
}

func TestRead_DetectsBinaryImagePDF(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "bin.dat", "abc\x00\x01\x02def")
	writeFile(t, ws, "img.png", "\x89PNG\r\n\x1a\n....")
	writeFile(t, ws, "doc.pdf", "%PDF-1.7\n....")

	for _, tc := range []struct{ path, want string }{
		{"bin.dat", "[binary file"},
		{"img.png", "[image"},
		{"doc.pdf", "[PDF"},
	} {
		r, err := m.read(context.Background(), mustJSON(map[string]any{"path": tc.path}))
		if err != nil || !r.Success {
			t.Fatalf("read %s: %v (%v)", tc.path, err, r.Error)
		}
		if got := r.Data.(string); !strings.HasPrefix(got, tc.want) {
			t.Errorf("read %s: want descriptor %q, got %q", tc.path, tc.want, got)
		}
	}
}

func TestRead_DirectoryReturnsTree(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "d/keep.txt", "x")
	writeFile(t, ws, "d/sub/deep.go", "package sub")
	res, err := m.read(context.Background(), mustJSON(map[string]any{"path": "d"}))
	if err != nil {
		t.Fatalf("reading a directory must NOT error (it returns a tree): %v", err)
	}
	out, _ := res.Data.(string)
	for _, want := range []string{"keep.txt", "sub/", "deep.go", "tree"} {
		if !strings.Contains(out, want) {
			t.Errorf("directory tree missing %q:\n%s", want, out)
		}
	}
}

func TestRead_DirectoryOutline(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "a.go", "package a\n\nfunc Alpha() {}\ntype Bravo struct{}\n")
	writeFile(t, ws, "sub/b.go", "package sub\n\nfunc Charlie() {}\n")
	writeFile(t, ws, "data.bin", "\x00\x01\x02not text")
	res, err := m.read(context.Background(), mustJSON(map[string]any{"path": ".", "outline": true}))
	if err != nil {
		t.Fatalf("outline of a directory must not error: %v", err)
	}
	out, _ := res.Data.(string)
	for _, want := range []string{"a.go", "func Alpha", "type Bravo", "sub/b.go", "func Charlie"} {
		if !strings.Contains(out, want) {
			t.Errorf("dir outline missing %q:\n%s", want, out)
		}
	}
}

// ---- glob : real ** recursion, skip-dirs, type, mtime sort, cap -----------

func TestGlob_DoubleStarRecursionAndSkipDirs(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "a.go", "x")
	writeFile(t, ws, "sub/b.go", "x")
	writeFile(t, ws, "sub/deep/c.go", "x")
	writeFile(t, ws, "node_modules/dep.go", "x") // must be skipped
	writeFile(t, ws, "README.md", "x")

	ctx := context.Background()
	check := func(pattern string, want int) []string {
		r, err := m.glob(ctx, mustJSON(map[string]any{"pattern": pattern}))
		if err != nil || !r.Success {
			t.Fatalf("glob %q: %v (%v)", pattern, err, r.Error)
		}
		files := r.Data.(map[string]any)["files"].([]string)
		if len(files) != want {
			t.Errorf("glob %q: want %d, got %d: %v", pattern, want, len(files), files)
		}
		return files
	}
	check("**/*.go", 3) // a.go, sub/b.go, sub/deep/c.go — NOT node_modules
	check("*.go", 1)    // top-level only
	check("sub/**/*.go", 2)
	check("**/*.md", 1)
	check("**/*.rs", 0)
}

// Models frequently key the pattern under the TOOL name ("glob") instead of the
// parameter name ("pattern"), which dead-ended the agent on "pattern must not be
// empty". The tool accepts that alias so a confused model can still list files.
func TestGlob_AcceptsToolNameAlias(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "a.go", "x")
	writeFile(t, ws, "sub/b.go", "x")
	ctx := context.Background()

	r, err := m.glob(ctx, mustJSON(map[string]any{"glob": "**/*.go"}))
	if err != nil || !r.Success {
		t.Fatalf("glob via {glob:...} alias failed: %v (%v)", err, r.Error)
	}
	if files := r.Data.(map[string]any)["files"].([]string); len(files) != 2 {
		t.Fatalf("alias glob want 2 files, got %d: %v", len(files), files)
	}
	// `pattern` still wins when both are present.
	r, _ = m.glob(ctx, mustJSON(map[string]any{"pattern": "*.go", "glob": "**/*.go"}))
	if files := r.Data.(map[string]any)["files"].([]string); len(files) != 1 {
		t.Fatalf("explicit pattern must win over glob alias, got %d: %v", len(files), files)
	}
	// Truly empty (neither key) still errors — no silent match-everything.
	if r, err := m.glob(ctx, mustJSON(map[string]any{})); err == nil && r.Success {
		t.Fatalf("empty glob must still error, got success")
	}
}

func TestGrep_AcceptsToolNameAlias(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "a.txt", "hello world\nfoo bar")
	ctx := context.Background()
	r, err := m.grep(ctx, mustJSON(map[string]any{"grep": "foo"}))
	if err != nil || !r.Success {
		t.Fatalf("grep via {grep:...} alias failed: %v (%v)", err, r.Error)
	}
}

func TestGlob_TypeFilter(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "dir1/x.txt", "x")
	writeFile(t, ws, "dir2/y.txt", "x")
	ctx := context.Background()

	dirs, _ := m.glob(ctx, mustJSON(map[string]any{"pattern": "**", "type": "dir"}))
	for _, f := range dirs.Data.(map[string]any)["files"].([]string) {
		if strings.HasSuffix(f, ".txt") {
			t.Errorf("type=dir returned a file: %s", f)
		}
	}
	files, _ := m.glob(ctx, mustJSON(map[string]any{"pattern": "**", "type": "file"}))
	for _, f := range files.Data.(map[string]any)["files"].([]string) {
		if f == "dir1" || f == "dir2" {
			t.Errorf("type=file returned a dir: %s", f)
		}
	}
}

func TestGlob_NewestFirstAndCap(t *testing.T) {
	m, ws := setupFS(t)
	base := time.Now().Add(-time.Hour)
	for i, n := range []string{"old.txt", "mid.txt", "new.txt"} {
		writeFile(t, ws, n, "x")
		// Stagger mtimes : old < mid < new.
		_ = os.Chtimes(filepath.Join(ws, n), base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	r, _ := m.glob(context.Background(), mustJSON(map[string]any{"pattern": "*.txt"}))
	files := r.Data.(map[string]any)["files"].([]string)
	if len(files) != 3 || files[0] != "new.txt" || files[2] != "old.txt" {
		t.Errorf("glob should be newest-first: %v", files)
	}

	// Cap + truncation flag.
	rc, _ := m.glob(context.Background(), mustJSON(map[string]any{"pattern": "*.txt", "max_results": 2}))
	d := rc.Data.(map[string]any)
	if len(d["files"].([]string)) != 2 || d["truncated"] != true {
		t.Errorf("cap not enforced: %v", d)
	}
}

// ---- write : atomic, created/overwrote, mode-preserving, no temp leftovers --

func TestWrite_AtomicCreateAndOverwrite(t *testing.T) {
	m, ws := setupFS(t)
	ctx := context.Background()

	r, err := m.write(ctx, mustJSON(map[string]any{"path": "sub/new.txt", "content": "hello"}))
	if err != nil || !r.Success {
		t.Fatalf("write create: %v (%v)", err, r.Error)
	}
	if r.Data.(map[string]any)["action"] != "created" {
		t.Errorf("first write should be 'created': %v", r.Data)
	}
	if got := readFile(t, ws, "sub/new.txt"); got != "hello" {
		t.Errorf("content = %q", got)
	}

	r2, _ := m.write(ctx, mustJSON(map[string]any{"path": "sub/new.txt", "content": "replaced"}))
	if r2.Data.(map[string]any)["action"] != "overwrote" {
		t.Errorf("second write should be 'overwrote': %v", r2.Data)
	}
	if got := readFile(t, ws, "sub/new.txt"); got != "replaced" {
		t.Errorf("overwrite content = %q", got)
	}

	// No temp files leaked into the directory.
	entries, _ := os.ReadDir(filepath.Join(ws, "sub"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dgt-") {
			t.Errorf("atomic write leaked a temp file: %s", e.Name())
		}
	}
}

// ---- matchGlob unit : doublestar semantics --------------------------------

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/y/a.go", true},
		{"*.go", "a.go", true},
		{"*.go", "x/a.go", false}, // single * never crosses /
		{"a/**/b", "a/b", true},   // ** matches zero segments
		{"a/**/b", "a/x/y/b", true},
		{"a/*", "a/b", true},
		{"a/*", "a/b/c", false},
		{"**", "anything/at/all", true},
		{"src/**/*.ts", "src/app/main.ts", true},
		{"src/**/*.ts", "lib/main.ts", false},

		// brace alternation — the bug that returned nothing to the agent.
		{"**/*.{js,ts}", "a.js", true},
		{"**/*.{js,ts}", "x/y/a.ts", true},
		{"**/*.{js,ts}", "a.py", false},
		{"*.{js,ts,jsx,tsx,py,java,cpp,c,go,rs}", "main.go", true},
		{"*.{js,ts,jsx,tsx,py,java,cpp,c,go,rs}", "main.rb", false},
		{"src/{a,b}/*.go", "src/a/x.go", true},
		{"src/{a,b}/*.go", "src/c/x.go", false},
		// nested + empty alternative.
		{"{a,b{c,d}}.txt", "bd.txt", true},
		{"file{,_old}.go", "file.go", true},
		{"file{,_old}.go", "file_old.go", true},
		// numeric / alpha ranges, step, zero-pad.
		{"img{1..3}.png", "img2.png", true},
		{"img{1..3}.png", "img4.png", false},
		{"v{1..9..2}.txt", "v5.txt", true},
		{"v{1..9..2}.txt", "v4.txt", false},
		{"f{01..10}.log", "f07.log", true},
		{"f{01..10}.log", "f7.log", false},
		{"{a..e}.md", "c.md", true},
		{"{a..e}.md", "g.md", false},

		// character classes : ranges, negation (both ! and ^), POSIX.
		{"[a-c].go", "b.go", true},
		{"[a-c].go", "d.go", false},
		{"[!a-c].go", "d.go", true},
		{"[!a-c].go", "b.go", false},
		{"[^x].txt", "a.txt", true},
		{"file[[:digit:]].txt", "file7.txt", true},
		{"file[[:digit:]].txt", "filex.txt", false},
		{"[[:alpha:]]*.go", "main.go", true},
		{"[[:alpha:]]*.go", "1main.go", false},

		// backslash escaping of glob metacharacters.
		{`a\*b.txt`, "a*b.txt", true},
		{`a\*b.txt`, "axb.txt", false},

		// extended globs.
		{"@(foo|bar).go", "foo.go", true},
		{"@(foo|bar).go", "baz.go", false},
		{"file?(s).txt", "file.txt", true},
		{"file?(s).txt", "files.txt", true},
		{"file?(s).txt", "filess.txt", false},
		{"a+(b).c", "abbb.c", true},
		{"a+(b).c", "a.c", false},
		{"a*(b).c", "a.c", true},
		{"a*(b).c", "abb.c", true},
		{"!(*.test).js", "app.js", true},
		{"!(*.test).js", "app.test.js", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
