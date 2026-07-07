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
	projectSummary(excludePath string) ProjectSummary
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
// the box if its server is installed. A spec with no matching server installed
// simply fails at start time and enters the failCooldown until the next try —
// declaring many specs is therefore cheap, NOT something the agent loop pays
// for on every turn. The table below covers the languages an agent is most
// likely to encounter; for the rest the user adds a one-line Config.Servers
// entry. Spec order matters when extensions overlap: app config wins via
// buildSpecs prepending; within builtins we put the unambiguous owner first.
func builtinSpecs() []serverSpec {
	s := func(name, cmd string, exts []string, markers ...string) serverSpec {
		return serverSpec{
			name: name, protocol: "lsp",
			argv: strings.Fields(cmd), extensions: exts, rootMarkers: markers,
		}
	}
	b := func(name string, exts []string, markers ...string) serverSpec {
		return serverSpec{
			name: name, protocol: "builtin",
			extensions: exts, rootMarkers: markers,
		}
	}
	return []serverSpec{
		// Systems & curly-brace
		s("go", "gopls", []string{".go"}, "go.mod", "go.work"),
		s("rust", "rust-analyzer", []string{".rs"}, "Cargo.toml"),
		s("c-cpp", "clangd",
			[]string{".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".m", ".mm"},
			"compile_commands.json", ".clangd", "CMakeLists.txt", "Makefile", "meson.build"),
		s("zig", "zls", []string{".zig"}, "build.zig"),

		// JVM & .NET
		s("java", "jdtls", []string{".java"}, "pom.xml", "build.gradle", "build.gradle.kts"),
		s("kotlin", "kotlin-language-server", []string{".kt", ".kts"}, "build.gradle.kts", "build.gradle", "pom.xml"),
		s("scala", "metals", []string{".scala", ".sc", ".sbt"}, "build.sbt", "build.sc"),
		s("csharp", "omnisharp -lsp", []string{".cs", ".csx"}, "global.json"),
		s("fsharp", "fsautocomplete --adaptive-lsp-server-enabled", []string{".fs", ".fsx", ".fsi"}),

		// Dynamic / scripting
		s("python", "pyright-langserver --stdio", []string{".py", ".pyi"},
			"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile"),
		s("typescript", "typescript-language-server --stdio",
			[]string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
			"tsconfig.json", "jsconfig.json", "package.json"),
		s("ruby", "solargraph stdio", []string{".rb"}, "Gemfile", ".solargraph.yml"),
		s("php", "intelephense --stdio", []string{".php"}, "composer.json"),
		s("bash", "bash-language-server start", []string{".sh", ".bash"}),
		s("lua", "lua-language-server", []string{".lua"}, ".luarc.json"),

		// Functional
		s("haskell", "haskell-language-server-wrapper --lsp", []string{".hs", ".lhs"}, "cabal.project", "stack.yaml"),
		s("elixir", "elixir-ls", []string{".ex", ".exs"}, "mix.exs"),
		s("erlang", "erlang_ls", []string{".erl", ".hrl"}, "rebar.config"),
		s("ocaml", "ocamllsp", []string{".ml", ".mli"}, "dune-project"),
		s("clojure", "clojure-lsp", []string{".clj", ".cljs", ".cljc", ".edn"}, "project.clj", "deps.edn"),
		s("elm", "elm-language-server", []string{".elm"}, "elm.json"),

		// Mobile
		s("swift", "sourcekit-lsp", []string{".swift"}, "Package.swift"),
		s("dart", "dart language-server --protocol=lsp", []string{".dart"}, "pubspec.yaml"),

		// Web — JSON/YAML/HTML/XML use builtin Go-native validators so an agent
		// gets syntax diagnostics with zero install. CSS still routes to the
		// external server (no good pure-Go validator that catches real CSS bugs).
		b("json", []string{".json", ".jsonc", ".excalidraw"}),
		b("yaml", []string{".yaml", ".yml"}),
		b("html", []string{".html", ".htm"}),
		b("xml", []string{".xml", ".xsd", ".xsl", ".svg"}),
		// Mermaid diagram source — a Go-native linter for the LLM-common
		// breakages (reserved words, fences, missing header, unbalanced
		// subgraph). Feeds the agent real-time diagnostics via lsp_diagnose.
		b("mermaid", []string{".mmd", ".mermaid"}),
		s("css", "vscode-css-language-server --stdio", []string{".css", ".scss", ".sass", ".less"}),

		// Markup
		s("latex", "texlab", []string{".tex", ".bib"}),
		s("markdown", "marksman server", []string{".md", ".markdown"}),

		// Infrastructure
		s("terraform", "terraform-ls serve", []string{".tf", ".tfvars"}, ".terraform"),
		s("nix", "nil", []string{".nix"}, "flake.nix"),
		s("dockerfile", "docker-langserver --stdio", []string{".dockerfile"}),
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
	if t, ok := m.failed[key]; ok {
		if time.Since(t) < failCooldown {
			m.mu.Unlock()
			return nil, fmt.Errorf("lsp: server %q recently failed to start; skipping (cooldown)", spec.name)
		}
		// Cooldown elapsed: drop the entry so the map cannot grow without bound
		// over a long-running daemon. Subsequent successes also delete it.
		delete(m.failed, key)
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
// live; "builtin" runs zero-install Go-native validators (JSON/YAML/HTML/XML)
// so the user gets syntax diagnostics out of the box without installing any
// language server. compiler/linter remain reserved extension points.
func startBackend(ctx context.Context, spec serverSpec, root string) (backend, error) {
	switch spec.protocol {
	case "lsp", "":
		ls := newLangServer(spec.name, root)
		if err := ls.start(ctx, spec.argv); err != nil {
			return nil, err
		}
		return ls, nil
	case "builtin":
		v, ok := builtinValidator(spec.name)
		if !ok {
			return nil, fmt.Errorf("lsp: unknown builtin validator %q", spec.name)
		}
		return newLiteBackend(spec.name, root, v), nil
	case "compiler", "linter":
		return nil, fmt.Errorf("lsp: %q backend not implemented yet (only lsp/builtin)", spec.protocol)
	default:
		return nil, fmt.Errorf("lsp: unknown protocol %q", spec.protocol)
	}
}

// builtinValidator maps a builtin spec name to its Go-native validator. Kept
// next to startBackend so the wiring stays in one place — adding a new builtin
// language is one entry here and one builtinSpecs row, no other touch.
func builtinValidator(name string) (func(string) []Diagnostic, bool) {
	switch name {
	case "json":
		return validateJSON, true
	case "yaml":
		return validateYAML, true
	case "html":
		return validateHTML, true
	case "xml":
		return validateXML, true
	case "mermaid":
		return validateMermaid, true
	}
	return nil, false
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

// projectSummary returns the language-server-wide rollup of every other file
// known to the backend that serves `path`. Empty when the project is clean or
// no backend is yet running for that path.
func (m *manager) projectSummary(ctx context.Context, path string) ProjectSummary {
	b, err := m.backendFor(ctx, path)
	if err != nil {
		return ProjectSummary{}
	}
	return b.projectSummary(path)
}

func (m *manager) stopAll(ctx context.Context) {
	m.mu.Lock()
	running := m.running
	m.running = map[string]backend{}
	m.mu.Unlock()
	// Stop backends in parallel: a single wedged server must not block the
	// teardown of healthy ones. Each backend.stop is itself bounded (see
	// client.close), so the overall wait is the slowest single backend, not
	// the sum across all of them.
	var wg sync.WaitGroup
	wg.Add(len(running))
	for _, b := range running {
		go func() {
			defer wg.Done()
			b.stop(ctx)
		}()
	}
	wg.Wait()
	m.cancel() // tear down the process-lifetime context
}

// universalRootMarkers are the last-resort markers used when the language-
// specific ones (go.mod, pyproject.toml, Cargo.toml, …) are not found while
// walking up. They identify the real project boundary so the LSP server is
// launched at a root from which it can SEE the file's siblings — otherwise it
// falls into single-file mode and every cross-file import looks "undefined"
// even though the other files exist on disk.
//
// The list is comprehensive on purpose: a project written in ANY supported
// language should be detected even when the spec's own markers are missing
// (loose script, undocumented project, monorepo mixing languages). VCS markers
// come first so they win when present — they identify the truest project
// boundary; the rest catch repos with a less common VCS or none at all.
var universalRootMarkers = []string{
	// Version control — the gold-standard signal for project root
	".git", ".hg", ".svn", ".fossil", "_FOSSIL_", "_darcs", ".bzr",

	// Multi-module / workspace manifests across the language spectrum
	"go.work", "Cargo.toml", "rust-project.json",
	"pyproject.toml", "setup.py", "setup.cfg", "Pipfile", "requirements.txt",
	"package.json", "tsconfig.json", "jsconfig.json",
	"pom.xml", "build.gradle", "build.gradle.kts", "build.sbt", "build.sc",
	"Gemfile", "composer.json", "mix.exs",
	"stack.yaml", "cabal.project",
	"Package.swift", "pubspec.yaml", "Project.toml", "Manifest.toml",
	"deps.edn", "project.clj", "build.zig", "v.mod",
	"rebar.config", "dune-project", "elm.json", "shard.yml",
	"flake.nix", "default.nix",
	"global.json", // .NET

	// Generic build / editor signals
	"Makefile", "GNUmakefile", "CMakeLists.txt", "meson.build", "configure.ac",
	".editorconfig",
}

// findRoot walks up from the file looking for a workspace marker. Language
// markers win on priority; VCS / generic markers are the fallback so a project
// without the language's own manifest still lands on the right root. Last
// resort is the file's own directory.
func findRoot(path string, markers []string) string {
	start := filepath.Dir(absOr(path))
	if r, ok := walkUpForMarkers(start, markers); ok {
		return r
	}
	if r, ok := walkUpForMarkers(start, universalRootMarkers); ok {
		return r
	}
	return start
}

// walkUpForMarkers walks from start up to the volume root, returning the first
// directory that contains ANY of the given markers. ok=false means none of the
// markers were found anywhere up the tree.
func walkUpForMarkers(start string, markers []string) (string, bool) {
	if len(markers) == 0 {
		return "", false
	}
	dir := start
	for {
		for _, mk := range markers {
			if _, err := os.Stat(filepath.Join(dir, mk)); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
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
