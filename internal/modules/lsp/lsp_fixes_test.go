package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ----- B1 — per-URI lock serializes didOpen / didChange ---------------------

// fakeServer is an LSP-side stub speaking the wire protocol over a pair of
// pipes. It records every method it receives so a test can assert the order in
// which the client sent didOpen / didChange.
type fakeServer struct {
	stdinR  io.ReadCloser
	stdoutW io.WriteCloser

	mu       sync.Mutex
	received []string
}

func newFakeServer(stdinR io.ReadCloser, stdoutW io.WriteCloser) *fakeServer {
	fs := &fakeServer{stdinR: stdinR, stdoutW: stdoutW}
	go fs.run()
	return fs
}

func (fs *fakeServer) run() {
	r := bufio.NewReader(fs.stdinR)
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
		switch msg.Method {
		case "initialize":
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"capabilities":{}}}`, *msg.ID)
			fmt.Fprintf(fs.stdoutW, "Content-Length: %d\r\n\r\n%s", len(body), body)
		case "textDocument/didOpen", "textDocument/didChange":
			fs.mu.Lock()
			fs.received = append(fs.received, msg.Method)
			fs.mu.Unlock()
		}
	}
}

func (fs *fakeServer) order() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.received...)
}

func TestB1_PerURILockOrdersDidOpenBeforeDidChange(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	fs := newFakeServer(stdinR, stdoutW)

	ls := newLangServer("fake", t.TempDir())
	ls.cl = newClientConn(stdinW, stdoutR, ls.onNotify)
	ls.posEncoding = "utf-8"
	t.Cleanup(func() {
		_ = stdoutW.Close()
		_ = stdinW.Close()
	})

	const path = "/x/y/z.go"
	const settle = 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fire N goroutines all targeting the same file. With the per-URI lock, the
	// exact protocol sequence must be [didOpen, didChange, didChange, ...] —
	// never a didChange before the first didOpen.
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			_, _ = ls.notifyChange(ctx, path, "package z\n", settle)
		}()
	}
	wg.Wait()

	got := fs.order()
	if len(got) != N {
		t.Fatalf("server received %d frames, want %d: %v", len(got), N, got)
	}
	if got[0] != "textDocument/didOpen" {
		t.Fatalf("first frame = %q, want didOpen — B1 reintroduced", got[0])
	}
	for i, m := range got[1:] {
		if m != "textDocument/didChange" {
			t.Errorf("frame[%d] = %q, want didChange", i+1, m)
		}
	}
	t.Logf("B1 fix verified: %d concurrent calls produced [didOpen, %dx didChange]", N, N-1)
}

// ----- B2 — async replyNull doesn't block the read loop ---------------------

func TestB2_ReplyNullDoesNotBlockReadLoop(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	notifyCh := make(chan string, 4)
	c := newClientConn(stdinW, stdoutR, func(method string, _ json.RawMessage) {
		notifyCh <- method
	})
	t.Cleanup(func() {
		_ = stdoutW.Close()
		_ = stdinW.Close()
		<-c.done
	})

	// The server sends a request that the client must replyNull to, then
	// immediately a notification. With B2 unfixed the read loop would block on
	// the stdin write (we are NOT draining stdinR), so the notification would
	// never be observed. With the fix the reply is dispatched to a goroutine
	// and the loop reads the next frame instantly.
	req := `{"jsonrpc":"2.0","id":42,"method":"workspace/configuration","params":{}}`
	note := `{"jsonrpc":"2.0","method":"keep/alive","params":{}}`
	go func() {
		fmt.Fprintf(stdoutW, "Content-Length: %d\r\n\r\n%s", len(req), req)
		fmt.Fprintf(stdoutW, "Content-Length: %d\r\n\r\n%s", len(note), note)
	}()

	select {
	case m := <-notifyCh:
		if m != "keep/alive" {
			t.Fatalf("got method %q, want keep/alive", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read loop blocked: notification never arrived — B2 reintroduced")
	}

	// Now let the orphaned replyNull goroutine drain so the cleanup pipe close
	// doesn't dead-lock it.
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()
}

// ----- B3 — UTF-16 columns are converted to byte columns --------------------

func TestB3_UTF16ColumnToByteColumn(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		utf16Col int
		want     int
	}{
		{"empty zero", "", 0, 0},
		{"ascii", "hello", 3, 3},
		{"past end", "abc", 100, 3},
		{"emoji surrogate pair: cursor right after emoji (2 utf16 units = 4 bytes)",
			"💡x", 2, 4},
		{"cursor after emoji + 1 ascii", "💡x", 3, 5},
		{"two emojis then x", "💡💡x", 4, 8},
		{"BMP char, cursor at é", "café", 3, 3},      // c,a,f = 3 bytes; cursor AT é
		{"BMP char, cursor past é", "café", 4, 5},    // 3 + 2 (é is 2 UTF-8 bytes)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := utf16ColumnToByteColumn(tc.line, tc.utf16Col)
			if got != tc.want {
				t.Errorf("utf16ColumnToByteColumn(%q, %d) = %d, want %d", tc.line, tc.utf16Col, got, tc.want)
			}
		})
	}
}

func TestB3_ToDiagnosticsBytes_ConvertsEmojiColumn(t *testing.T) {
	// File: "💡 fixme\n" — the bug is on column AFTER the emoji.
	// Server (UTF-16) says Character = 2 (after the surrogate pair).
	// Byte-correct column for the agent: 5 (1-based, past the 4-byte emoji).
	content := "💡 fixme\n"
	raw := []lspDiagnostic{
		{Range: lspRange{Start: lspPosition{Line: 0, Character: 2}}, Severity: 2, Message: "trailing whitespace"},
	}
	got := toDiagnosticsBytes("x.go", raw, content, "utf-16")
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Column != 5 {
		t.Errorf("UTF-16 conversion: column = %d, want 5 (byte-accurate past the emoji)", got[0].Column)
	}

	// When the server speaks utf-8 we must NOT convert (column is already a byte).
	got8 := toDiagnosticsBytes("x.go", raw, content, "utf-8")
	if got8[0].Column != 3 { // 2 + 1 (1-based), raw character
		t.Errorf("utf-8 path should not convert: column = %d, want 3", got8[0].Column)
	}

	// When the content is unknown we must fall back to the raw value.
	gotNoContent := toDiagnosticsBytes("x.go", raw, "", "utf-16")
	if gotNoContent[0].Column != 3 {
		t.Errorf("no-content fallback: column = %d, want 3 (raw)", gotNoContent[0].Column)
	}
}

// ----- B4 — UNC paths produce RFC 8089 file://host/share/... ----------------

func TestB4_UNCPathToURI(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("UNC paths are Windows-only")
	}
	cases := []struct {
		in   string
		want string
	}{
		{`\\server\share\file.go`, "file://server/share/file.go"},
		{`\\srv\sh\sub dir\f.go`, "file://srv/sh/sub%20dir/f.go"},
		{`//server/share/file.go`, "file://server/share/file.go"},
	}
	for _, tc := range cases {
		got := pathToURI(tc.in)
		if got != tc.want {
			t.Errorf("pathToURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// pickCaseVariantPaths returns two absolute paths that name the SAME logical
// file with different casing, in the syntax the local OS understands. Used to
// reproduce B5 (double-didOpen) across case-insensitive filesystems.
func pickCaseVariantPaths() (string, string) {
	if runtime.GOOS == "windows" {
		return `C:\proj\foo.go`, `c:\proj\foo.go`
	}
	// POSIX (macOS, Linux): vary only the case of an interior segment.
	return "/Proj/Foo.go", "/proj/foo.go"
}

// ----- B5 — case-folding on case-insensitive filesystems --------------------

// Same logical file with different casing must trigger ONE didOpen + one
// didChange, never two didOpens. Reproduces on Windows (always case-insensitive)
// and macOS (APFS / HFS+ default).
func TestB5_MixedCaseSendsSingleDidOpen(t *testing.T) {
	if !isCaseInsensitiveFS() {
		t.Skip("case-folding fix is for case-insensitive filesystems (Windows / macOS)")
	}
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	fs := newFakeServer(stdinR, stdoutW)

	ls := newLangServer("fake", t.TempDir())
	ls.cl = newClientConn(stdinW, stdoutR, ls.onNotify)
	ls.posEncoding = "utf-8"
	t.Cleanup(func() {
		_ = stdoutW.Close()
		_ = stdinW.Close()
	})

	const settle = 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pick paths the local OS understands so filepath.Abs doesn't mangle them.
	pathA, pathB := pickCaseVariantPaths()
	_, _ = ls.notifyChange(ctx, pathA, "package x\n", settle)
	_, _ = ls.notifyChange(ctx, pathB, "package x\n", settle)

	got := fs.order()
	if len(got) != 2 {
		t.Fatalf("server received %d frames, want 2: %v", len(got), got)
	}
	if got[0] != "textDocument/didOpen" || got[1] != "textDocument/didChange" {
		t.Errorf("B5 reintroduced: got %v, want [didOpen, didChange]", got)
	}
}

