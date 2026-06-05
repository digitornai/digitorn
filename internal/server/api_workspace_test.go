package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonQuote encodes s as a JSON string literal (escaping Windows backslashes).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestWorkspace_GuessLanguage(t *testing.T) {
	cases := map[string]string{
		"a.ts": "typescript", "b.tsx": "typescript",
		"c.js": "javascript", "d.mjs": "javascript",
		"e.py": "python", "f.go": "go", "g.rs": "rust",
		"h.json": "json", "i.yaml": "yaml", "j.yml": "yaml",
		"k.md": "markdown", "l.css": "css", "m.html": "html",
		"sub/dir/file.go": "go", "Dockerfile": "", "noext": "", "weird.xyz": "",
	}
	for path, want := range cases {
		if got := guessLanguage(path); got != want {
			t.Errorf("guessLanguage(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestWorkspace_IsBinaryContent(t *testing.T) {
	if isBinaryContent([]byte("plain text\nsecond line")) {
		t.Error("text flagged as binary")
	}
	if !isBinaryContent([]byte{0x7f, 'E', 'L', 'F', 0x00, 0x01}) {
		t.Error("NUL-bearing content not flagged binary")
	}
	if isBinaryContent(nil) {
		t.Error("empty content flagged binary")
	}
	// A NUL beyond the 8 KiB scan window is not inspected (cheap heuristic).
	big := append([]byte(strings.Repeat("x", 9000)), 0x00)
	if isBinaryContent(big) {
		t.Error("NUL past the 8 KiB window should not be scanned")
	}
}

func TestWorkspace_IsShadowRel(t *testing.T) {
	shadow := []string{".digitorn", ".digitorn/git/HEAD", "./.digitorn/objects/ab", ".digitorn\\git\\HEAD"}
	for _, p := range shadow {
		if !isShadowRel(p) {
			t.Errorf("isShadowRel(%q) = false, want true", p)
		}
	}
	ok := []string{"src/main.go", ".digitornrc", ".gitignore", "a/.digitorn-notes.md", "package.json"}
	for _, p := range ok {
		if isShadowRel(p) {
			t.Errorf("isShadowRel(%q) = true, want false", p)
		}
	}
}

func TestWorkspace_ReadFileCapped(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, trunc, err := readFileCapped(small, 1024)
	if err != nil || trunc || string(data) != "hello" {
		t.Fatalf("small: data=%q trunc=%v err=%v", data, trunc, err)
	}
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	data, trunc, err = readFileCapped(big, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc || len(data) != 1000 {
		t.Fatalf("big: expected truncated=true len=1000, got trunc=%v len=%d", trunc, len(data))
	}
}

// TestWorkspace_FileRoute_Isolation drives the real getWorkspaceFile handler
// through the API router against a session whose workdir is a temp dir, proving
// the cardinal isolation rule end-to-end: a session reads ONLY its own files,
// the daemon shadow repo is hidden, escapes/missing/dirs are rejected, and a
// DIFFERENT user is forbidden. include_baseline is omitted so the handler never
// touches the (test-absent) module servicebus.
func TestWorkspace_FileRoute_Isolation(t *testing.T) {
	h := newAPIHarness(t)
	h.mux.With(authMiddleware).Get(
		"/api/apps/{app_id}/sessions/{session_id}/workspace/files/*",
		h.daemon.getWorkspaceFile,
	)

	// Owned session for user-A, pinned to a temp workdir. The workdir lands on
	// the session state via the create event's meta → projection, exactly as in
	// production; we read it back so file writes target the resolved path.
	tmp := t.TempDir()
	createBody := `{"title":"t","workdir":` + jsonQuote(tmp) + `}`
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", createBody)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, string(body))
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)
	wd, _ := created["workdir"].(string)
	if wd == "" {
		t.Fatalf("session got no workdir: %s", string(body))
	}

	mustWrite(t, filepath.Join(wd, "hello.txt"), "hi there")
	mustWrite(t, filepath.Join(wd, "sub", "nested.go"), "package sub\n")
	mustWrite(t, filepath.Join(wd, ".digitorn", "git", "HEAD"), "ref: refs/heads/master\n")
	if err := os.WriteFile(filepath.Join(wd, "bin.dat"), []byte{0x00, 0x01, 0x02, 'X'}, 0o644); err != nil {
		t.Fatal(err)
	}

	base := "/api/apps/app-1/sessions/" + sid + "/workspace/files/"

	// Valid read.
	code, body = h.do(t, "GET", base+"hello.txt", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("valid read: %d %s", code, string(body))
	}
	var got struct {
		Payload struct {
			Content  string `json:"content"`
			Language string `json:"language"`
			Binary   bool   `json:"binary"`
		} `json:"payload"`
	}
	decodeBody(t, body, &got)
	if got.Payload.Content != "hi there" {
		t.Fatalf("content = %q", got.Payload.Content)
	}

	// Nested path, percent-encoded exactly as the browser sends it.
	if code, _ = h.do(t, "GET", base+"sub%2Fnested.go", "user-A", ""); code != http.StatusOK {
		t.Fatalf("nested %%2F read: %d", code)
	}

	// Binary file → served with an empty body + binary flag, never raw bytes.
	code, body = h.do(t, "GET", base+"bin.dat", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("binary read: %d", code)
	}
	decodeBody(t, body, &got)
	if !got.Payload.Binary || got.Payload.Content != "" {
		t.Fatalf("binary not flagged: binary=%v content=%q", got.Payload.Binary, got.Payload.Content)
	}

	// Shadow repo → forbidden.
	if code, _ = h.do(t, "GET", base+".digitorn%2Fgit%2FHEAD", "user-A", ""); code != http.StatusForbidden {
		t.Fatalf("shadow path: expected 403, got %d", code)
	}

	// Missing file → 404.
	if code, _ = h.do(t, "GET", base+"nope.txt", "user-A", ""); code != http.StatusNotFound {
		t.Fatalf("missing: expected 404, got %d", code)
	}

	// Directory → 400.
	if code, _ = h.do(t, "GET", base+"sub", "user-A", ""); code != http.StatusBadRequest {
		t.Fatalf("dir: expected 400, got %d", code)
	}

	// Workdir escape → denied (403 from PathPolicy, or 404 if the router
	// collapses the .. segments first — either way the file is NEVER served).
	if code, _ = h.do(t, "GET", base+"..%2F..%2F..%2F..%2Fetc%2Fpasswd", "user-A", ""); code == http.StatusOK {
		t.Fatalf("escape was served (code %d) — isolation breach", code)
	}

	// CARDINAL RULE: a DIFFERENT user can never reach this session's files.
	if code, _ = h.do(t, "GET", base+"hello.txt", "user-B", ""); code != http.StatusForbidden {
		t.Fatalf("cross-user: expected 403, got %d", code)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
