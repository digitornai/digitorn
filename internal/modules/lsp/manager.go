package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// backend is the extensible slot: anything that can turn a file change into
// diagnostics. Today only the LSP backend (langServer) implements it; the
// compiler (go build / cargo) and linter (ruff / eslint) backends — and a
// future Word-extraction backend — plug in here behind the same interface,
// without touching the manager or the module's tools.
type backend interface {
	notifyChange(ctx context.Context, path, content string, settle time.Duration) ([]Diagnostic, error)
	diagnosticsFor(path string) []Diagnostic
	stop(ctx context.Context)
}

// serverSpec declares how to launch a backend for a set of extensions.
type serverSpec struct {
	name        string
	protocol    string // "lsp" (compiler/linter reserved)
	argv        []string
	extensions  []string
	rootMarkers []string
}

// manager routes a file to the right backend by extension, lazily starting one
// backend instance per (spec, workspace root) and caching it.
type manager struct {
	mu      sync.Mutex
	specs   []serverSpec
	running map[string]backend   // key: spec.name + "\x00" + root
	failed  map[string]time.Time // key -> last failed-start time
	settle  time.Duration

	// baseCtx is the LIFETIME of the spawned language-server processes. They
	// must outlive any single request, so servers are started against this
	// context (not a per-call one): a per-call ctx would let exec.CommandContext
	// kill the server the moment the first request finishes.
	baseCtx context.Context
	cancel  context.CancelFunc
}

// failCooldown throttles restart attempts for a server that just failed to
// start, so a broken/missing server is not re-spawned on every edit (no
// process storm) — failures stay cheap and the loop is never disturbed.
const failCooldown = 30 * time.Second

func newManager(specs []serverSpec, settle time.Duration) *manager {
	if settle <= 0 {
		settle = 10 * time.Second // generous: a cold Node-based server's first push can take seconds
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &manager{
		specs: specs, running: map[string]backend{}, failed: map[string]time.Time{},
		settle: settle, baseCtx: ctx, cancel: cancel,
	}
}

// builtinSpecs are the zero-config defaults: a language gets diagnostics out of
// the box if its server is installed. gopls covers Go (our own stack).
func builtinSpecs() []serverSpec {
	return []serverSpec{
		{name: "go", protocol: "lsp", argv: []string{"gopls"}, extensions: []string{".go"}, rootMarkers: []string{"go.mod", "go.work"}},
		{name: "python", protocol: "lsp", argv: []string{"pyright-langserver", "--stdio"}, extensions: []string{".py", ".pyi"}, rootMarkers: []string{"pyproject.toml", "setup.py", "requirements.txt"}},
		{name: "typescript", protocol: "lsp", argv: []string{"typescript-language-server", "--stdio"}, extensions: []string{".ts", ".tsx", ".js", ".jsx"}, rootMarkers: []string{"tsconfig.json", "package.json"}},
		{name: "rust", protocol: "lsp", argv: []string{"rust-analyzer"}, extensions: []string{".rs"}, rootMarkers: []string{"Cargo.toml"}},
		{name: "latex", protocol: "lsp", argv: []string{"texlab"}, extensions: []string{".tex", ".bib"}},
	}
}

// specFor returns the spec whose extensions include the file's extension.
func (m *manager) specFor(path string) (serverSpec, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	for _, s := range m.specs {
		for _, e := range s.extensions {
			if strings.EqualFold(e, ext) {
				return s, true
			}
		}
	}
	return serverSpec{}, false
}

// backendFor returns a running backend for the file, starting one if needed.
func (m *manager) backendFor(ctx context.Context, path string) (backend, error) {
	spec, ok := m.specFor(path)
	if !ok {
		return nil, fmt.Errorf("no language server configured for %q", filepath.Ext(path))
	}
	root := findRoot(path, spec.rootMarkers)
	key := spec.name + "\x00" + root

	m.mu.Lock()
	if b, ok := m.running[key]; ok {
		m.mu.Unlock()
		return b, nil
	}
	if t, ok := m.failed[key]; ok && time.Since(t) < failCooldown {
		m.mu.Unlock()
		return nil, fmt.Errorf("lsp: server %q recently failed to start; skipping (cooldown)", spec.name)
	}
	m.mu.Unlock()

	// Start against baseCtx (server lifetime), NOT ctx (this request) — else the
	// server is killed when this first request's context is cancelled.
	b, err := startBackend(m.baseCtx, spec, root)
	if err != nil {
		m.mu.Lock()
		m.failed[key] = time.Now()
		m.mu.Unlock()
		return nil, err
	}

	m.mu.Lock()
	// Re-check: another goroutine may have started one while we were spawning.
	if existing, ok := m.running[key]; ok {
		m.mu.Unlock()
		b.stop(ctx)
		return existing, nil
	}
	delete(m.failed, key)
	m.running[key] = b
	m.mu.Unlock()
	return b, nil
}

// startBackend builds the backend for a spec's protocol. The LSP backend is
// live; compiler/linter are the reserved extension points.
func startBackend(ctx context.Context, spec serverSpec, root string) (backend, error) {
	switch spec.protocol {
	case "lsp", "":
		ls := newLangServer(spec.name, root)
		if err := ls.start(ctx, spec.argv); err != nil {
			return nil, err
		}
		return ls, nil
	case "compiler", "linter":
		return nil, fmt.Errorf("lsp: %q backend not implemented yet (only lsp)", spec.protocol)
	default:
		return nil, fmt.Errorf("lsp: unknown protocol %q", spec.protocol)
	}
}

func (m *manager) notifyChange(ctx context.Context, path, content string) ([]Diagnostic, error) {
	b, err := m.backendFor(ctx, path)
	if err != nil {
		return nil, err
	}
	return b.notifyChange(ctx, path, content, m.settle)
}

func (m *manager) diagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	b, err := m.backendFor(ctx, path)
	if err != nil {
		return nil, err
	}
	return b.diagnosticsFor(path), nil
}

func (m *manager) stopAll(ctx context.Context) {
	m.mu.Lock()
	running := m.running
	m.running = map[string]backend{}
	m.mu.Unlock()
	for _, b := range running {
		b.stop(ctx)
	}
	m.cancel() // tear down the process-lifetime context
}

// findRoot walks up from the file looking for a workspace marker; falls back to
// the file's directory.
func findRoot(path string, markers []string) string {
	dir := filepath.Dir(absOr(path))
	if len(markers) == 0 {
		return dir
	}
	for {
		for _, mk := range markers {
			if _, err := os.Stat(filepath.Join(dir, mk)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Dir(absOr(path)) // hit volume root with no marker
		}
		dir = parent
	}
}

func absOr(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
