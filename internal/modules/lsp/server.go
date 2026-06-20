package lsp

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

	// posEncoding is the position encoding the server agreed to during the
	// initialize handshake. "utf-8" if the server supports it (gopls,
	// rust-analyzer recent), else "utf-16" (LSP default — pyright, ts-server).
	// Read once during start, then read-only — no lock needed.
	posEncoding string

	mu        sync.Mutex
	opened    map[string]bool            // cacheKey(uri) -> didOpen sent
	diags     map[string][]lspDiagnostic // cacheKey(uri) -> latest raw diagnostics
	content   map[string]string          // cacheKey(uri) -> latest content (for utf-16→bytes conversion)
	waiters   map[string]chan struct{}   // cacheKey(uri) -> signal on next publish
	fileOrder []string                   // insertion order of tracked files, for FIFO eviction

	// uriLocks serializes the (state update + protocol write) sequence per file:
	// without it, two concurrent notifyChange calls on the same URI could let G2
	// observe opened=true (set by G1) and send didChange BEFORE G1 won the write
	// mutex and sent didOpen — a protocol violation. One mutex per URI scales
	// with file count, same as the rest of the per-file maps; never cleaned up
	// for the lifetime of the server (bounded by reachable file set).
	uriLocks sync.Map // map[string]*sync.Mutex
}

const (
	// settleGrace is how long we wait for a refining follow-up publish after a
	// non-empty result, so analysis that lands in two pushes is caught.
	settleGrace = 250 * time.Millisecond
	// emptyFollowupWindow is how long an empty first push waits for a possible
	// non-empty follow-up before the file is treated as genuinely clean.
	emptyFollowupWindow = 800 * time.Millisecond
	// maxTrackedFiles caps the number of files the per-file maps hold so a
	// long-running daemon that touches many files cannot grow them unbounded.
	// FIFO eviction is good enough here — an agent is generally working on a
	// hot set; cold files re-open transparently on the next notifyChange.
	maxTrackedFiles = 2048
)

func newLangServer(name, root string) *langServer {
	return &langServer{
		name:        name,
		root:        root,
		posEncoding: "utf-16", // LSP default until the server says otherwise
		opened:      map[string]bool{},
		diags:       map[string][]lspDiagnostic{},
		content:     map[string]string{},
		waiters:     map[string]chan struct{}{},
	}
}

// lockURI returns a function that releases an exclusive lock on the given
// (normalized) URI key. Two concurrent operations on the same file serialize;
// operations on different files run unhindered.
func (s *langServer) lockURI(key string) func() {
	m, _ := s.uriLocks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// trackFileLocked records a new file in the eviction order and prunes the
// oldest entries when the cap is exceeded. Caller MUST hold s.mu.
func (s *langServer) trackFileLocked(key string) {
	if s.opened[key] {
		// already tracked; nothing to do — we only record first encounters so
		// the eviction order reflects insertion, not last touch.
		return
	}
	s.fileOrder = append(s.fileOrder, key)
	for len(s.fileOrder) > maxTrackedFiles {
		victim := s.fileOrder[0]
		s.fileOrder = s.fileOrder[1:]
		delete(s.opened, victim)
		delete(s.diags, victim)
		delete(s.content, victim)
		// Keep uriLocks alive: a concurrent caller may still hold it; the entry
		// is harmless (one bare sync.Mutex). It will be re-used if the file
		// comes back. Same for waiters (always nil at this point).
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
			"general": map[string]any{
				"positionEncodings": []string{"utf-8", "utf-16"},
			},
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{"relatedInformation": true},
				"synchronization":    map[string]any{"didSave": true, "didOpen": true, "didChange": true},
			},
		},
		"workspaceFolders":    []map[string]any{{"uri": rootURI, "name": filepath.Base(s.root)}},
		"initializationOptions": lspInitOptions(s.name, s.root),
	}
	res, err := cl.call(ctx, "initialize", initParams)
	if err != nil {
		cl.close(ctx)
		return err
	}
	// Read the negotiated position encoding. If the server omits it (pre-3.17),
	// the protocol default is utf-16 — already set in newLangServer.
	var initResp struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(res, &initResp); err == nil && initResp.Capabilities.PositionEncoding != "" {
		s.posEncoding = initResp.Capabilities.PositionEncoding
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

	// Per-URI lock: the (state-mutation + wire-write) pair must be atomic w.r.t.
	// any other operation on the SAME file, so didOpen always precedes didChange
	// at the server even under concurrent callers. Different files run unimpeded.
	unlock := s.lockURI(key)
	defer unlock()

	wait := make(chan struct{}, 1)
	s.mu.Lock()
	s.waiters[key] = wait
	first := !s.opened[key]
	s.trackFileLocked(key) // must run BEFORE opened[key]=true (it uses opened to detect first encounters)
	s.opened[key] = true
	s.content[key] = content
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
// Columns are converted from the negotiated position encoding (UTF-16 by
// default) to byte columns when the file content is known, so callers always
// see byte-accurate positions regardless of which encoding the server speaks.
func (s *langServer) diagnosticsFor(path string) []Diagnostic {
	key := cacheKey(pathToURI(path))
	s.mu.Lock()
	raw := s.diags[key]
	content := s.content[key]
	s.mu.Unlock()
	return toDiagnosticsBytes(path, raw, content, s.posEncoding)
}

// ProjectSummary is a server-wide rollup of every file's current diagnostics,
// so an agent that touched one file sees how the WHOLE project stands after
// the edit — not just the edited file in isolation. Empty when nothing is
// broken anywhere across the workspace the language server is analyzing.
type ProjectSummary struct {
	TotalErrors   int            `json:"total_errors"`
	TotalWarnings int            `json:"total_warnings"`
	AffectedFiles []AffectedFile `json:"affected_files,omitempty"`
}

// AffectedFile is one entry of the project rollup.
type AffectedFile struct {
	File     string `json:"file"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
}

// projectSummary aggregates EVERY file the server has analyzed, excluding the
// one the caller is already showing on its own — so the agent reads "this
// edit's diagnostics" plus "what else changed elsewhere as a result" without
// double-counting. Files are sorted by error count desc, then path asc, so
// the rendered output is stable.
func (s *langServer) projectSummary(excludePath string) ProjectSummary {
	excludeKey := cacheKey(pathToURI(excludePath))
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := ProjectSummary{}
	for key, raw := range s.diags {
		if key == excludeKey {
			continue
		}
		errs, warns := 0, 0
		for _, d := range raw {
			switch d.Severity {
			case 1:
				errs++
			case 2:
				warns++
			}
		}
		if errs == 0 && warns == 0 {
			continue
		}
		sum.TotalErrors += errs
		sum.TotalWarnings += warns
		sum.AffectedFiles = append(sum.AffectedFiles, AffectedFile{
			File:     s.keyToDisplayPath(key),
			Errors:   errs,
			Warnings: warns,
		})
	}
	sort.Slice(sum.AffectedFiles, func(i, j int) bool {
		if sum.AffectedFiles[i].Errors != sum.AffectedFiles[j].Errors {
			return sum.AffectedFiles[i].Errors > sum.AffectedFiles[j].Errors
		}
		return sum.AffectedFiles[i].File < sum.AffectedFiles[j].File
	})
	return sum
}

// keyToDisplayPath turns a cacheKey ("file:///abs/x.go") into a path the agent
// can read at a glance: relative to the server's workspace root when possible,
// falling back to the absolute path. Forward slashes everywhere so the same
// string renders identically on every OS.
func (s *langServer) keyToDisplayPath(key string) string {
	p := strings.TrimPrefix(key, "file://")
	if len(p) > 3 && p[0] == '/' && p[2] == ':' { // Windows: /C:/x → C:/x
		p = p[1:]
	}
	if rel, err := filepath.Rel(s.root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

func (s *langServer) stop(ctx context.Context) {
	if s.cl != nil {
		s.cl.close(ctx)
	}
}

// pathToURI converts a filesystem path to an RFC 8089 file URI. Three shapes:
//
//   - POSIX absolute        /tmp/x.go            → file:///tmp/x.go
//   - Windows drive         C:\foo\x.go          → file:///C:/foo/x.go
//   - Windows UNC share     \\host\share\x.go    → file://host/share/x.go
//
// UNC paths are special: filepath.Abs on Windows rewrites them relative to the
// current drive, silently producing a wrong local path. We detect UNC upstream
// and skip Abs entirely so the share host becomes the URI host (RFC 8089 §3,
// "non-empty authority" form), the way every LSP server expects.
func pathToURI(p string) string {
	if runtime.GOOS == "windows" {
		if rest, ok := stripUNCPrefix(p); ok {
			return "file://" + encodeURIPath(filepath.ToSlash(rest))
		}
	}
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

// stripUNCPrefix accepts both \\host\share\... and //host/share/... forms and
// returns the remainder past the two leading separators. ok=false means the
// path is not a UNC path and should be handled normally.
func stripUNCPrefix(p string) (string, bool) {
	if len(p) < 3 {
		return "", false
	}
	if (p[0] == '\\' && p[1] == '\\') || (p[0] == '/' && p[1] == '/') {
		return p[2:], true
	}
	return "", false
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
// ("file:///c%3A/x"). Percent-decoding collapses both to the same form. On
// case-insensitive filesystems (Windows always, macOS by default — APFS/HFS+),
// we also lowercase so File.go and file.go map to the same entry; otherwise a
// caller passing the path with different casing would trigger a spurious
// second didOpen on the same logical file. On Linux (case-sensitive ext4/xfs/
// btrfs/zfs) we preserve case — two files differing only in case ARE distinct.
func cacheKey(uri string) string {
	if dec, err := url.PathUnescape(uri); err == nil {
		uri = dec
	}
	if isCaseInsensitiveFS() {
		return strings.ToLower(uri)
	}
	return uri
}

// isCaseInsensitiveFS reports whether the OS default filesystem treats paths
// case-insensitively. Conservative: only the OSes whose stock filesystem is
// case-insensitive in factory state. A user who reformatted their macOS volume
// as case-sensitive will see at worst two files differing only in case collapse
// in the LSP cache — an extremely rare layout in real codebases.
func isCaseInsensitiveFS() bool {
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	default:
		return false
	}
}

// languageID maps a file extension to the LSP languageId the server expects.
// Coverage matches builtinSpecs so any builtin server gets a meaningful tag,
// not "plaintext" (which would silence analysis even when the server is up).
// Unknown extensions still fall back to "plaintext".
func languageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".c":
		return "c"
	case ".h":
		return "c" // ambiguous with C++ headers; tools tolerate either tag
	case ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".m":
		return "objective-c"
	case ".mm":
		return "objective-cpp"
	case ".zig":
		return "zig"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".scala", ".sc", ".sbt":
		return "scala"
	case ".cs", ".csx":
		return "csharp"
	case ".fs", ".fsx", ".fsi":
		return "fsharp"
	case ".py", ".pyi":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".sh", ".bash":
		return "shellscript"
	case ".lua":
		return "lua"
	case ".hs", ".lhs":
		return "haskell"
	case ".ex", ".exs":
		return "elixir"
	case ".erl", ".hrl":
		return "erlang"
	case ".ml", ".mli":
		return "ocaml"
	case ".clj", ".cljs", ".cljc", ".edn":
		return "clojure"
	case ".elm":
		return "elm"
	case ".swift":
		return "swift"
	case ".dart":
		return "dart"
	case ".tex":
		return "latex"
	case ".bib":
		return "bibtex"
	case ".json", ".jsonc":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md", ".markdown":
		return "markdown"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".scss":
		return "scss"
	case ".sass":
		return "sass"
	case ".less":
		return "less"
	case ".tf", ".tfvars":
		return "terraform"
	case ".nix":
		return "nix"
	case ".dockerfile":
		return "dockerfile"
	case ".xml", ".xsd", ".xsl":
		return "xml"
	case ".svg":
		return "svg"
	default:
		return "plaintext"
	}
}

func readFileText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// lspInitOptions builds server-specific initializationOptions that exclude
// directories listed in the workdir's .gitignore. This prevents heavy language
// servers (tsserver, pyright) from indexing vendored or generated trees that
// are irrelevant to the agent's work — regardless of the repo's content.
// Each language server has its own option keys; unknown servers get empty opts.
func lspInitOptions(serverName, root string) map[string]any {
	excluded := gitignoreExcludedDirs(root)
	if len(excluded) == 0 {
		return nil
	}
	switch serverName {
	case "typescript":
		// tsserver watchOptions: prevents loading AND watching gitignored dirs.
		// Works on any repo without creating config files.
		globs := make([]string, len(excluded))
		for i, d := range excluded {
			globs[i] = "**/" + d
		}
		return map[string]any{
			"tsserver": map[string]any{
				"watchOptions": map[string]any{
					"excludeDirectories": globs,
				},
			},
			"preferences": map[string]any{
				"autoImportFileExcludePatterns": globs,
			},
		}
	case "python":
		// Pyright: exclude gitignored dirs from analysis.
		patterns := make([]string, len(excluded))
		for i, d := range excluded {
			patterns[i] = d + "/**"
		}
		return map[string]any{
			"python": map[string]any{
				"analysis": map[string]any{
					"exclude": patterns,
				},
			},
		}
	}
	return nil
}

// gitignoreExcludedDirs parses the root .gitignore and returns simple directory
// names (no wildcards, no path separators) that should be excluded from LSP
// indexing. Only top-level dir-only entries are returned since those are the
// ones that cause runaway memory usage in language servers.
func gitignoreExcludedDirs(root string) []string {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		name := strings.TrimSuffix(line, "/")
		if strings.ContainsAny(name, "*?[/\\") {
			continue
		}
		out = append(out, name)
	}
	return out
}
