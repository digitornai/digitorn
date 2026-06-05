package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---- framing ---------------------------------------------------------------

func TestReadFrame(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{}}`
	raw := "Content-Length: " + fmt.Sprint(len(body)) + "\r\n\r\n" + body
	got, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != body {
		t.Errorf("frame = %q, want %q", got, body)
	}
}

func TestReadFrame_MissingLength(t *testing.T) {
	if _, err := readFrame(bufio.NewReader(strings.NewReader("\r\n\r\n{}"))); err == nil {
		t.Error("expected error on missing Content-Length")
	}
}

// ---- URI -------------------------------------------------------------------

func TestPathToURI(t *testing.T) {
	got := pathToURI("/tmp/a b/x.go")
	if runtime.GOOS != "windows" {
		if got != "file:///tmp/a%20b/x.go" {
			t.Errorf("uri = %q", got)
		}
	} else {
		// On Windows the path is made absolute against the drive; just assert shape.
		if !strings.HasPrefix(got, "file:///") || !strings.Contains(got, "%20") {
			t.Errorf("windows uri = %q", got)
		}
	}
	if cacheKey("file:///A") == cacheKey("file:///a") && runtime.GOOS != "windows" {
		t.Error("cacheKey should be case-sensitive off Windows")
	}
}

// ---- diagnostics conversion ------------------------------------------------

func TestToDiagnostics(t *testing.T) {
	raw := []lspDiagnostic{
		{Range: lspRange{Start: lspPosition{Line: 4, Character: 8}}, Severity: 1, Message: "boom", Source: "compiler", Code: json.RawMessage(`"E001"`)},
		{Range: lspRange{Start: lspPosition{Line: 0, Character: 0}}, Severity: 2, Message: "warn", Code: json.RawMessage(`123`)},
		{Range: lspRange{Start: lspPosition{Line: 1, Character: 1}}, Severity: 0, Message: "unknown-sev"},
	}
	got := toDiagnostics("x.go", raw)
	if got[0].Line != 5 || got[0].Column != 9 {
		t.Errorf("0-based not converted to 1-based: %+v", got[0])
	}
	if got[0].Severity != "error" || got[0].Code != "E001" {
		t.Errorf("diag[0] = %+v", got[0])
	}
	if got[1].Severity != "warning" || got[1].Code != "123" {
		t.Errorf("diag[1] = %+v", got[1])
	}
	if got[2].Severity != "error" { // unknown severity must fail loud, not silently downgrade
		t.Errorf("unknown severity = %q, want error", got[2].Severity)
	}
}

// ---- spec routing ----------------------------------------------------------

func TestSpecForAndBuildSpecs(t *testing.T) {
	c := Config{Servers: map[string]ServerConfig{
		"customgo": {Command: "my-go-lsp --stdio", Extensions: []string{".go"}},
	}}
	specs := buildSpecs(c)
	m := newManager(specs, time.Second)
	s, ok := m.specFor("/p/main.go")
	if !ok || s.name != "customgo" { // app config wins over builtin gopls
		t.Errorf("specFor(.go) = %+v ok=%v, want customgo", s, ok)
	}
	if _, ok := m.specFor("/p/x.unknownext"); ok {
		t.Error("unknown extension should not match")
	}
	// builtins still present for uncovered languages.
	if _, ok := m.specFor("/p/x.rs"); !ok {
		t.Error("builtin rust spec missing")
	}
}

func TestFindRoot(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findRoot(filepath.Join(sub, "f.go"), []string{"go.mod"})
	if got != dir {
		t.Errorf("findRoot = %q, want %q", got, dir)
	}
	// No marker → file's own dir.
	if got := findRoot(filepath.Join(sub, "f.go"), []string{"nope.marker"}); got != sub {
		t.Errorf("findRoot fallback = %q, want %q", got, sub)
	}
}

// ---- client round-trip over an in-memory pipe (no subprocess) --------------

func TestClientRoundTrip(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	// Fake server: answer any request with {"pong":true} and push one notification.
	go func() {
		r := bufio.NewReader(stdinR)
		for {
			frame, err := readFrame(r)
			if err != nil {
				return
			}
			var msg struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(frame, &msg)
			if msg.ID != nil {
				writeRaw(stdoutW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"pong":true}}`, *msg.ID))
				writeRaw(stdoutW, `{"jsonrpc":"2.0","method":"test/note","params":{"hi":1}}`)
			}
		}
	}()

	noteCh := make(chan string, 1)
	c := newClientConn(stdinW, stdoutR, func(method string, _ json.RawMessage) {
		select {
		case noteCh <- method:
		default:
		}
	})
	t.Cleanup(func() { _ = stdinW.Close(); _ = stdoutW.Close(); <-c.done })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.call(ctx, "ping", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(res), `"pong":true`) {
		t.Errorf("result = %s", res)
	}
	select {
	case m := <-noteCh:
		if m != "test/note" {
			t.Errorf("notification method = %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Error("did not receive server notification")
	}
}

func writeRaw(w io.Writer, s string) {
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(s), s)
}

// ---- REAL gopls integration (skips if gopls is not installed) --------------

func TestGoplsDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed; skipping real LSP integration test")
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644))
	file := filepath.Join(dir, "main.go")
	// A real type error: assigning a string to an int.
	content := "package main\n\nfunc main() {\n\tvar x int = \"not an int\"\n\t_ = x\n}\n"
	must(t, os.WriteFile(file, []byte(content), 0o644))

	mgr := newManager(builtinSpecs(), 20*time.Second)
	defer mgr.stopAll(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	diags, err := mgr.notifyChange(ctx, file, content)
	if err != nil {
		t.Fatalf("notifyChange: %v", err)
	}
	t.Logf("gopls returned %d diagnostic(s):", len(diags))
	for _, d := range diags {
		t.Logf("  %s:%d:%d [%s] %s (%s)", filepath.Base(d.File), d.Line, d.Column, d.Severity, d.Message, d.Source)
	}
	if len(diags) == 0 {
		t.Fatal("expected at least one diagnostic from gopls for a type error")
	}
	hasError := false
	for _, d := range diags {
		if d.Severity == "error" {
			hasError = true
		}
	}
	if !hasError {
		t.Errorf("expected an error-severity diagnostic, got %+v", diags)
	}

	// Second call on the SAME server (the agent edits then re-checks). This is
	// the flow that failed live with "pipe is being closed" when the server was
	// tied to the first request's context. After the fix the process survives
	// and the corrected file reports clean.
	fixed := "package main\n\nfunc main() {\n\tvar x int = 42\n\t_ = x\n}\n"
	must(t, os.WriteFile(file, []byte(fixed), 0o644))
	diags2, err := mgr.notifyChange(ctx, file, fixed)
	if err != nil {
		t.Fatalf("second notifyChange failed (server died?): %v", err)
	}
	if len(diags2) != 0 {
		t.Errorf("expected 0 diagnostics after fix, got %+v", diags2)
	}
	t.Logf("second call OK: server survived, fixed file is clean")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
