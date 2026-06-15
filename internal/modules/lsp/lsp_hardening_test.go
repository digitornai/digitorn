package lsp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ----- B9 — flexContent refuses malformed input instead of silently corrupting

func TestB9_FlexContent_RefusesObjectWithNoStringField(t *testing.T) {
	// Object array element with none of the recognized keys: the old behavior
	// json-marshaled it and fed the result to the language server as source
	// code. The new behavior errors so the LLM sees its mistake.
	cases := []struct {
		name string
		body string
	}{
		{"array with bad object", `["good line", {"src": "x"}]`},
		{"object with no known key", `{"random": "x"}`},
		{"array with bool", `["a", true]`},
		{"array with number", `["a", 42]`},
		{"top-level number", `42`},
		{"top-level bool", `true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f flexContent
			err := f.UnmarshalJSON([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error for malformed input %q, got %q", tc.body, string(f))
			}
			if !strings.Contains(err.Error(), "lsp:") {
				t.Errorf("error should be lsp-prefixed for clarity: %v", err)
			}
		})
	}
}

func TestB9_FlexContent_StillAcceptsKnownShapes(t *testing.T) {
	// The flexibility for shapes LLMs actually emit must NOT regress.
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain string", `"hello\nworld"`, "hello\nworld"},
		{"null", `null`, ""},
		{"line array (strings)", `["a", "b", "c"]`, "a\nb\nc"},
		{"line array (objects with text)", `[{"text":"a"},{"text":"b"}]`, "a\nb"},
		{"line array (objects with content)", `[{"content":"a"}]`, "a"},
		{"object with content", `{"content":"hello"}`, "hello"},
		{"object with body", `{"body":"hello"}`, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f flexContent
			if err := f.UnmarshalJSON([]byte(tc.body)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(f) != tc.want {
				t.Errorf("got %q, want %q", string(f), tc.want)
			}
		})
	}
}

// ----- B6 — client.close is bounded; cmd.Wait can never wedge it ------------

// blockingProcess simulates a child that ignores SIGKILL by never letting Wait
// return: we drop the cmd pointer so close() treats the client as cmd-less,
// then verify the close still returns within the deadline.
func TestB6_CloseIsBounded(t *testing.T) {
	// Build a client around in-memory pipes; no real subprocess is involved.
	// The server side never reads stdin and never closes stdout: the polite
	// shutdown write would otherwise block forever. close() must respect the
	// caller's deadline regardless.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
	})

	c := newClientConn(stdinW, stdoutR, func(string, json.RawMessage) {})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.close(ctx)
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("close blocked for %v, want bounded by ctx (200ms + grace)", elapsed)
	}
	t.Logf("close returned in %v under a 200ms deadline", elapsed)
}

// ----- B7/B8 — file map eviction prevents unbounded growth ------------------

func TestB7_FIFOEvictionCapsTrackedFiles(t *testing.T) {
	ls := newLangServer("fake", t.TempDir())
	// Insert maxTrackedFiles + N entries directly via the locked helper. We
	// don't need a live server; we test the bookkeeping in isolation.
	const overflow = 100
	for i := range maxTrackedFiles + overflow {
		key := keyForIndex(i)
		ls.mu.Lock()
		ls.trackFileLocked(key)
		ls.opened[key] = true
		ls.diags[key] = nil
		ls.content[key] = "x"
		ls.mu.Unlock()
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if got := len(ls.opened); got != maxTrackedFiles {
		t.Errorf("opened len = %d, want %d", got, maxTrackedFiles)
	}
	if got := len(ls.diags); got != maxTrackedFiles {
		t.Errorf("diags len = %d, want %d", got, maxTrackedFiles)
	}
	if got := len(ls.content); got != maxTrackedFiles {
		t.Errorf("content len = %d, want %d", got, maxTrackedFiles)
	}
	// The oldest `overflow` entries must be gone, the newest must be present.
	if _, ok := ls.opened[keyForIndex(overflow-1)]; ok {
		t.Error("oldest entry should have been evicted")
	}
	if _, ok := ls.opened[keyForIndex(maxTrackedFiles+overflow-1)]; !ok {
		t.Error("newest entry should be retained")
	}
}

func TestB8_FailedCooldownIsPurgedAfterElapsed(t *testing.T) {
	m := newManager(nil, time.Second)
	m.specs = []serverSpec{{
		name:        "fake",
		protocol:    "lsp",
		argv:        []string{"definitely-not-on-path-xyz"},
		extensions:  []string{".xyz"},
		rootMarkers: nil,
	}}

	// Compute the key exactly the way backendFor does: spec name + NUL + root,
	// where root for a marker-less spec is the file's parent dir resolved to
	// an absolute path.
	const path = "/p/file.xyz"
	abs, _ := filepath.Abs(path)
	staleKey := "fake\x00" + filepath.Dir(abs)

	staleTimestamp := time.Now().Add(-2 * failCooldown)
	m.mu.Lock()
	m.failed[staleKey] = staleTimestamp
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := m.backendFor(ctx, path)
	if err == nil {
		t.Fatal("expected start to fail (bogus binary)")
	}

	// Two acceptable post-conditions, both proving lazy purge:
	//   1. The stale entry was deleted and a FRESH failure was recorded.
	//   2. The stale entry was deleted and not re-inserted (unlikely here).
	// The original stale timestamp must NOT survive.
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts, exists := m.failed[staleKey]; exists && ts.Equal(staleTimestamp) {
		t.Error("stale failed-entry survived lookup — lazy purge did not run")
	}
}

// ----- B13 — findRoot falls back to the VCS root, not the file's directory --

// Without the fix the LSP server would launch with rootUri = file's dir, which
// puts gopls / pyright / ts-server into single-file mode: every cross-file
// import is then reported as "undefined" even though the sibling files exist.
// The fix walks up to .git (and friends) so the server sees the real project.

func TestB13_FindRoot_FallsBackToGitRoot(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(proj, "deep", "sub", "leaf")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "x.go")

	// No go.mod anywhere — the language-specific marker MUST miss. The VCS
	// fallback then lands the LSP server on the real project root.
	got := findRoot(file, []string{"go.mod"})
	if got != proj {
		t.Errorf("findRoot = %q, want %q (.git fallback)", got, proj)
	}
}

func TestB13_FindRoot_LanguageMarkerStillWinsOverVCS(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A nested sub-module: closer go.mod must beat the outer .git so gopls
	// gets the right module boundary, not the whole monorepo.
	inner := filepath.Join(proj, "services", "api")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(inner, "main.go")

	got := findRoot(file, []string{"go.mod"})
	if got != inner {
		t.Errorf("findRoot = %q, want %q (closer go.mod beats outer .git)", got, inner)
	}
}

func TestB13_FindRoot_NoMarkerAnywhereStillFallsToFileDir(t *testing.T) {
	// A pristine dir with NOTHING above it (TempDir lives under the OS temp
	// folder which has no .git). Behaviour must remain the file's directory —
	// no surprise jump to C:\ or /home.
	dir := t.TempDir()
	file := filepath.Join(dir, "x.go")
	got := findRoot(file, []string{"never.gonna.find.this"})
	if got != dir {
		t.Errorf("findRoot = %q, want %q (no marker anywhere → file's dir)", got, dir)
	}
}

// ----- Cross-platform: cacheKey case-folding semantics ----------------------

// Pin the per-OS behavior so a future edit cannot silently flip it. Lowercase
// folding on case-insensitive filesystems (Windows always, macOS by default);
// case-sensitive otherwise — collapsing case on ext4 would merge two distinct
// files into one cache entry.
func TestCrossPlatform_CacheKeyCaseFolding(t *testing.T) {
	a := cacheKey("file:///A/B")
	b := cacheKey("file:///a/b")
	switch runtime.GOOS {
	case "windows", "darwin":
		if a != b {
			t.Errorf("on %s cacheKey must fold case: %q vs %q", runtime.GOOS, a, b)
		}
	default:
		if a == b {
			t.Errorf("on %s cacheKey must preserve case: %q vs %q", runtime.GOOS, a, b)
		}
	}
}

// pathToURI must produce the same shape on every OS for POSIX-absolute paths,
// so a Linux daemon and a macOS daemon report the same URI for /tmp/x.go.
// Windows-specific shapes (drive letters, UNC) live in TestB4/TestPathToURI.
func TestCrossPlatform_PathToURI_POSIXShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-shape test does not apply on Windows")
	}
	got := pathToURI("/tmp/x.go")
	want := "file:///tmp/x.go"
	if got != want {
		t.Errorf("pathToURI(POSIX abs) = %q, want %q on %s", got, want, runtime.GOOS)
	}
	if got := pathToURI("/tmp/a b/x.go"); got != "file:///tmp/a%20b/x.go" {
		t.Errorf("pathToURI(space) = %q, want %%20 encoding", got)
	}
}

// ----- B14 — comprehensive language coverage --------------------------------

// Every builtin spec must (a) be routable from at least one extension, (b) map
// that extension to a non-"plaintext" languageID, and (c) declare a sensible
// fallback root (markers OR rely on VCS — the universal fallback covers this
// when the rootMarkers list is intentionally empty for header-less languages).
// This test guards against silently breaking language coverage when someone
// edits builtinSpecs or languageID.
func TestB14_EveryBuiltinHasARoutableExtensionAndLanguageID(t *testing.T) {
	specs := builtinSpecs()
	if len(specs) < 20 {
		t.Fatalf("expected broad language coverage, got only %d builtin specs", len(specs))
	}
	seenLanguage := map[string]bool{}
	for _, sp := range specs {
		if len(sp.extensions) == 0 {
			t.Errorf("spec %q has no extensions — unroutable", sp.name)
			continue
		}
		for _, ext := range sp.extensions {
			lang := languageID("file" + ext)
			if lang == "plaintext" {
				t.Errorf("spec %q claims extension %q but languageID returns plaintext", sp.name, ext)
			}
			seenLanguage[lang] = true
		}
	}
	t.Logf("builtin coverage: %d specs spanning %d distinct languageIDs", len(specs), len(seenLanguage))
}

// Quick check that the common file types every agent will encounter route to
// a spec, even without any user-supplied Config.Servers.
func TestB14_CommonExtensionsRouteToASpec(t *testing.T) {
	common := []string{
		".go", ".rs", ".c", ".cpp", ".h", ".hpp", ".py", ".ts", ".tsx", ".js",
		".jsx", ".java", ".kt", ".scala", ".cs", ".rb", ".php", ".sh", ".lua",
		".hs", ".ex", ".erl", ".ml", ".clj", ".elm", ".swift", ".dart",
		".html", ".css", ".json", ".yaml", ".md", ".tex", ".tf", ".nix",
	}
	m := newManager(builtinSpecs(), time.Second)
	missing := []string{}
	for _, ext := range common {
		if _, ok := m.specFor("/proj/file" + ext); !ok {
			missing = append(missing, ext)
		}
	}
	if len(missing) > 0 {
		t.Errorf("extensions with no builtin server: %v", missing)
	}
}

// ----- helpers --------------------------------------------------------------

func keyForIndex(i int) string {
	return "file:///k/" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

