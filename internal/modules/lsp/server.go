package lsp

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// langServer wraps one running language-server process for a given workspace
// root: it owns the handshake, document sync, and a cache of the diagnostics
// the server pushes per file.
type langServer struct {
	name string
	root string
	cl   *client

	mu      sync.Mutex
	opened  map[string]bool            // uri -> didOpen sent
	diags   map[string][]lspDiagnostic // cacheKey(uri) -> latest diagnostics
	waiters map[string]chan struct{}   // cacheKey(uri) -> signal on next publish
}

const (
	// settleGrace is how long we wait for a refining follow-up publish after a
	// non-empty result, so analysis that lands in two pushes is caught.
	settleGrace = 250 * time.Millisecond
	// emptyFollowupWindow is how long an empty first push waits for a possible
	// non-empty follow-up before the file is treated as genuinely clean.
	emptyFollowupWindow = 800 * time.Millisecond
)

func newLangServer(name, root string) *langServer {
	return &langServer{
		name:    name,
		root:    root,
		opened:  map[string]bool{},
		diags:   map[string][]lspDiagnostic{},
		waiters: map[string]chan struct{}{},
	}
}

// start spawns the server against baseCtx — the process lives until baseCtx is
// cancelled (manager teardown), independent of any request. The handshake is
// bounded by its own timeout so a wedged startup fails fast without killing or
// leaking the process.
func (s *langServer) start(baseCtx context.Context, argv []string) error {
	cl, err := startClient(baseCtx, argv, s.root, s.onNotify)
	if err != nil {
		return err
	}
	s.cl = cl

	ctx, cancel := context.WithTimeout(baseCtx, 15*time.Second)
	defer cancel()

	rootURI := pathToURI(s.root)
	initParams := map[string]any{
		"processId": nil,
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{"relatedInformation": true},
				"synchronization":    map[string]any{"didSave": true, "didOpen": true, "didChange": true},
			},
		},
		"workspaceFolders": []map[string]any{{"uri": rootURI, "name": filepath.Base(s.root)}},
	}
	if _, err := cl.call(ctx, "initialize", initParams); err != nil {
		cl.close(ctx)
		return err
	}
	if err := cl.notify("initialized", map[string]any{}); err != nil {
		cl.close(ctx)
		return err
	}
	return nil
}

func (s *langServer) onNotify(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}
	var p publishDiagnosticsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	key := cacheKey(p.URI)
	s.mu.Lock()
	s.diags[key] = p.Diagnostics
	if w := s.waiters[key]; w != nil {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

// notifyChange opens or updates the document and returns the diagnostics the
// server reports for it, waiting up to settle for the push.
func (s *langServer) notifyChange(ctx context.Context, path, content string, settle time.Duration) ([]Diagnostic, error) {
	uri := pathToURI(path)
	key := cacheKey(uri)

	wait := make(chan struct{}, 1)
	s.mu.Lock()
	s.waiters[key] = wait
	first := !s.opened[uri]
	s.opened[uri] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters, key)
		s.mu.Unlock()
	}()

	if first {
		if err := s.cl.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID(path), "version": 1, "text": content,
			},
		}); err != nil {
			return nil, err
		}
	} else {
		if err := s.cl.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": 2},
			"contentChanges": []map[string]any{{"text": content}},
		}); err != nil {
			return nil, err
		}
	}

	// Wait for the server to analyze and push diagnostics. `settle` bounds the
	// wait for the FIRST push — generous, because a cold server (e.g. a
	// Node-based one) can take seconds to start before its first publish.
	select {
	case <-wait:
	case <-time.After(settle):
		return s.diagnosticsFor(path), nil // never analyzed (server down / too slow)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Got the first push. If it already carries findings, return them after a
	// brief grace for any refining follow-up. If it is empty, the file is clean
	// SO FAR — wait one short window for a possible non-empty follow-up before
	// concluding it is genuinely clean, so a clean file returns promptly instead
	// of stalling for the full settle.
	if len(s.diagnosticsFor(path)) == 0 {
		select {
		case <-wait:
		case <-time.After(emptyFollowupWindow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return s.diagnosticsFor(path), nil
	}
	select {
	case <-time.After(settleGrace):
	case <-ctx.Done():
	}
	return s.diagnosticsFor(path), nil
}

// diagnosticsFor returns the cached diagnostics for a file (empty if none).
func (s *langServer) diagnosticsFor(path string) []Diagnostic {
	key := cacheKey(pathToURI(path))
	s.mu.Lock()
	raw := s.diags[key]
	s.mu.Unlock()
	return toDiagnostics(path, raw)
}

func (s *langServer) stop(ctx context.Context) {
	if s.cl != nil {
		s.cl.close(ctx)
	}
}

// pathToURI converts a filesystem path to an RFC 8089 file URI. Backslashes
// become forward slashes and a Windows drive path gets the leading slash
// ("C:/x" -> "file:///C:/x") so the URI matches what the server echoes back —
// the mismatch the Python comment warned about.
func pathToURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	s := filepath.ToSlash(abs)
	if len(s) >= 2 && s[1] == ':' { // Windows drive
		s = "/" + s
	}
	return "file://" + encodeURIPath(s)
}

// encodeURIPath percent-encodes a path for a URI, leaving '/' and ':' and the
// unreserved set literal (matching VS Code / gopls conventions).
func encodeURIPath(s string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '/' || c == ':' || c == '-' || c == '_' || c == '.' || c == '~' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&0xf])
	}
	return b.String()
}

// cacheKey normalizes a URI for map lookups so the key we store (from the
// server's publishDiagnostics) matches the key we look up (from a file path),
// regardless of how the server encoded it. Servers differ: gopls emits a
// literal drive colon ("file:///C:/x"), pyright percent-encodes it
// ("file:///c%3A/x"). Percent-decoding collapses both to the same form; on
// Windows we also lowercase (case-insensitive filesystem).
func cacheKey(uri string) string {
	if dec, err := url.PathUnescape(uri); err == nil {
		uri = dec
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(uri)
	}
	return uri
}

// languageID maps a file extension to the LSP languageId.
func languageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py", ".pyi":
		return "python"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc":
		return "cpp"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".tex":
		return "latex"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".html":
		return "html"
	case ".css":
		return "css"
	default:
		return "plaintext"
	}
}

// readFileText reads a file as UTF-8 text (best-effort), for when the hook did
// not supply content.
func readFileText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
