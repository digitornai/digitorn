// Package server wires every component of the daemon and exposes Start/Stop
// for the binary entrypoint.
package server

import (
	"context"
	"fmt"
	"log/slog"
	nethttp "net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lmittmann/tint"

	"github.com/digitornai/digitorn/internal/adapters/transport/http"
	"github.com/digitornai/digitorn/internal/adapters/transport/realtime/socketio"
	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/config"
	"github.com/digitornai/digitorn/internal/core/eventbus"
	"github.com/digitornai/digitorn/internal/core/servicebus"
	"github.com/digitornai/digitorn/internal/credentials"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/mcphub"
	"github.com/digitornai/digitorn/internal/mcpservers"
	"github.com/digitornai/digitorn/internal/middlewareplugin"
	"github.com/digitornai/digitorn/internal/module/gateway"
	"github.com/digitornai/digitorn/internal/modules/pieces"
	"github.com/digitornai/digitorn/internal/modulesettings"
	"github.com/digitornai/digitorn/internal/persistence/db"
	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/provision"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/agent"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/blobstore"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/contextsvc"
	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/mediagen"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/skills"
	"github.com/digitornai/digitorn/internal/runtime/turn"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
	"github.com/digitornai/digitorn/internal/tokenizer"
	"github.com/digitornai/digitorn/internal/userskills"
	"github.com/digitornai/digitorn/internal/usersnippets"
	"github.com/digitornai/digitorn/internal/worker"
	"github.com/digitornai/digitorn/pkg/module"

	"gorm.io/gorm"
)

// Daemon is the assembled daemon ready to Start.
type Daemon struct {
	cfg     *config.Config
	logger  *slog.Logger
	httpSrv *http.Server
	rt      ports.RealtimeServer
	bus     ports.ServiceBus
	modules *module.Registry
	gdb     *gorm.DB

	sessionStore    *sessionstore.Bus
	sessionFlusher  *sessionstore.DiskFlusher
	sessionPaths    sessionstore.Paths
	envelopeBuilder *sessionstore.EnvelopeBuilder
	bridge          *SocketIOBridge
	workspaceLive   *workspaceLive   // debounced workspace-change notifier (also reused by the REST file save)
	blobStore       *blobstore.Store // content-addressed attachment store (multimodal in + out)
	officeConverter *officeConverter // bounded, off-path LibreOffice pptx/docx/xlsx → PDF preview

	jwks          *JWKS
	jwtVerifier   *JWTVerifier
	authValidator AuthValidator

	workerMgr         *worker.Manager
	llmClient         *llm.Client
	embeddingsClient  *embeddings.Client
	tokenizerClient   *tokenizer.Client
	serviceGateway    *gateway.Server                       // worker→daemon service bridge (embeddings, …)
	gatewayAddr       string                                // gateway loopback addr handed to worker pools
	gatewaySecret     string                                // gateway's dedicated handshake secret
	contextBG         atomic.Pointer[contextsvc.Background] // background EXACT context recompute (CTX-7)
	summaryMaintainer *summaryMaintainer                    // background high-fidelity summary, off the loop (CTX-8); nil when flag off
	contextTracker    *contextsvc.Tracker                   // freshest per-session context variable (engine + hooks read it)
	compactor         *contextCompactor                     // shared compactor — also used for reactive compaction on model switch
	ctxParts          sync.Map                              // sessionID -> ctxParts (build-time system+tools for the recount breakdown)
	agents            *agent.Manager

	appCompiler    *compiler.Compiler
	appMgr         appmgr.Manager
	provisioner    *provision.Provisioner // downloads app `requirements:` binaries (consent-gated, async)
	userSkills     *userskills.Store
	userSnippets   *usersnippets.Store
	mcpCatalog     *mcpCatalog           // materializes worker-hosted MCP tools as native actions
	piecesCatalog  *piecesCatalog        // materializes pieces bridge tools as native actions
	mcpOAuth       *mcpoauth.Service     // daemon-side MCP OAuth (token store, CSRF flow); nil if the key file is unavailable
	managedMCP     *mcpservers.Store     // per-user managed MCP server store (install/list/configure); nil if the key file is unavailable
	creds          *credentials.Store    // per-user encrypted credential vault; nil if the key file is unavailable
	credResolver   *credentials.Resolver // runtime O(1) per-user BYOK key cache; nil if the key file is unavailable
	moduleSettings *modulesettings.Store // per-user per-app module config deltas (BYOK); nil if the key file is unavailable
	mcpHub         *mcphub.Client        // read-only client for the Hub's curated MCP catalog (the install-config source)

	engine           runtime.Runner
	promptBuilder    *wiring.Builder
	sessionRunner    *sessionRunner // serializes + coalesces agent turns per session (user + proactive wakes)
	lifecycle        lifecycleFirer // fires session_end / pre_compact hooks outside the turn loop
	approvalRegistry *approval.Registry
	background       *background.Manager
	eventBus         ports.EventBus

	secretsOnce sync.Once
	secrets     *secretStore

	// modelDefaults stores per-user per-app per-agent default models, applied
	// to new sessions at creation (in-session switching still overrides).
	modelDefaults *modelDefaultsStore

	previewSecret     []byte // process-wide HMAC key for iframe-loadable preview tokens
	previewSecretOnce sync.Once
	previewLastKey    sync.Map                           // session root -> last pushed (entry|mtime) — dedup web_preview:attached
	activePreview     atomic.Pointer[activePreviewState] // build the iframe currently shows — serves its root-absolute assets (/assets/*)

	once sync.Once
}

// Build constructs the daemon from a loaded config. No I/O is started.
func Build(cfg *config.Config) (*Daemon, error) {
	logger := newLogger(cfg.Logging)

	gdb, err := db.Open(db.Options{
		Driver:          cfg.Database.Driver,
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
		LogLevel:        cfg.Database.LogLevel,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: db: %w", err)
	}
	if err := db.AutoMigrate(gdb); err != nil {
		return nil, fmt.Errorf("bootstrap: migrate: %w", err)
	}

	flusher, store, paths, err := buildSessionStore(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: session store: %w", err)
	}

	bus := servicebus.New()
	evtBus := eventbus.New(logger)
	mods := module.Default.WithBus(busAdapter{bus: bus})

	httpSrv := http.New(http.Options{
		Addr:            cfg.Server.Host + ":" + strconv.Itoa(cfg.Server.Port),
		ReadTimeout:     cfg.Server.ReadTimeout,
		WriteTimeout:    cfg.Server.WriteTimeout,
		ShutdownTimeout: cfg.Server.ShutdownTimeout,
		CORSOrigins:     cfg.Server.CORSOrigins,
	}, logger)

	// The embedded-preview iframe is served from the daemon's OWN origin and
	// opens its socket back to it — so that origin must be allowed even though
	// it isn't a configured client origin. Always allow-list the daemon's own
	// host:port (both 127.0.0.1 and localhost forms) for the socket handshake.
	socketOrigins := append([]string{}, cfg.Server.CORSOrigins...)
	for _, h := range []string{cfg.Server.Host, "127.0.0.1", "localhost"} {
		if h == "" {
			continue
		}
		socketOrigins = append(socketOrigins,
			fmt.Sprintf("http://%s:%d", h, cfg.Server.Port))
	}

	siosrv := socketio.New(socketio.Options{
		Path:                     "/socket.io/",
		Namespace:                "/events",
		PingInterval:             cfg.SocketIO.PingInterval,
		PingTimeout:              cfg.SocketIO.PingTimeout,
		ConnectTimeout:           cfg.SocketIO.ConnectTimeout,
		MaxHTTPBufferSize:        cfg.SocketIO.MaxHTTPBufferSize,
		AllowedOrigins:           socketOrigins,
		RedisURL:                 cfg.SocketIO.RedisURL,
		MaxDisconnectionDuration: 2 * time.Minute,
	}, logger)

	httpSrv.Router().Handle("/socket.io/*", siosrv.Handler())

	builder := sessionstore.NewEnvelopeBuilder(cfg.Sessions.InstanceID, cfg.Sessions.Capabilities)

	// Auth wiring. JWKS+JWT only if Auth.Enabled. Otherwise NullAuth dev mode.
	var (
		jwks        *JWKS
		jwtVerifier *JWTVerifier
		authVal     AuthValidator = NullAuth{}
	)
	if cfg.Auth.Enabled && cfg.Auth.Issuer != "" {
		jwks = NewJWKS(JWKSConfig{
			Issuer:          cfg.Auth.Issuer,
			JWKSURL:         cfg.Auth.JWKSURL,
			RefreshInterval: cfg.Auth.JWKSRefreshInterval,
			FailureBackoff:  cfg.Auth.JWKSFailureBackoff,
			Logger:          logger,
		})
		jwtVerifier = NewJWTVerifier(jwks, cfg.Auth.Issuer, cfg.Auth.Audience, cfg.Auth.UserIDClaim, cfg.Auth.JWTLeeway)
		authVal = &JWTAuthValidator{Verifier: jwtVerifier, DevMode: cfg.Auth.DevMode}
	}

	bridge := NewSocketIOBridge(siosrv, store, builder, paths, authVal, logger)

	wmgr := worker.NewManager(logger)

	// App manager : compiler wired to a RegistrySource that exposes
	// every in-proc registered module's manifest. Without this any
	// app declaring `agents[].modules: [filesystem]` (or any module)
	// fails install with "unknown module" — the compiler validates
	// references against a catalog, and an empty catalog has nothing
	// to validate against.
	// Worker-hosted modules also register their manifest in the
	// registry at startup (via the ProxyModule). Their proxy manifests
	// arrive asynchronously, so we ALSO validate against the static
	// module manifest files (manifests/) — this makes worker modules
	// (rag, database, …) known regardless of proxy-registration timing.
	compileSources := []catalog.ManifestSource{catalog.RegistrySource{Registry: mods}}
	for _, dir := range manifestDirsFor() {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			compileSources = append(compileSources, catalog.DirSource{Dir: dir})
		}
	}
	appCompiler := compiler.New().WithSources(compileSources...)
	appRoot := cfg.Apps.Root
	if !filepath.IsAbs(appRoot) {
		if abs, err := filepath.Abs(appRoot); err == nil {
			appRoot = abs
		}
	}
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		return nil, fmt.Errorf("bootstrap: mkdir apps root: %w", err)
	}
	am, err := appmgr.New(appmgr.Config{
		DB:       gdb,
		Root:     appRoot,
		Compiler: appCompiler,
		Logger:   logger,
		Channel:  cfg.Apps.Channel,
		Hub: appmgr.HubConfig{
			URL:             cfg.Apps.Hub.URL,
			Timeout:         cfg.Apps.Hub.Timeout,
			VerifySSL:       cfg.Apps.Hub.VerifySSL,
			MaxArchiveBytes: cfg.Apps.Hub.MaxArchiveBytes,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: appmgr: %w", err)
	}

	// MCP OAuth: a process-wide sealer over a key file backs the encrypted token
	// store. A key-file failure disables OAuth (handlers 503) without blocking boot.
	var mcpOAuth *mcpoauth.Service
	var mcpSrvStore *mcpservers.Store
	var credStore *credentials.Store
	var credResolver *credentials.Resolver
	var moduleSettings *modulesettings.Store
	var appSecrets *secretStore
	if sealer, serr := mcpoauth.NewSealer(mcpoauth.DefaultKeyPath()); serr != nil {
		logger.Warn("bootstrap: mcp oauth + managed servers + credential vault disabled (key file unavailable)", slog.String("error", serr.Error()))
		pieces.Setup(gdb, nil) // pieces still work but credentials stored unencrypted
	} else {
		mcpOAuth = mcpoauth.NewService(gdb, sealer)
		mcpSrvStore = mcpservers.NewStore(gdb, sealer) // shares the process sealer
		credStore = credentials.NewStore(gdb, sealer)  // per-user vault, same process sealer
		credResolver = credentials.NewResolver(credStore)
		moduleSettings = modulesettings.NewStore(gdb, sealer)
		appSecrets = newPersistedSecretStore(gdb, sealer)
		pieces.Setup(gdb, sealer)
	}

	// The daemon key (for the hub's daemon-only system-config endpoint) is read
	// via os.Getenv downstream; surface the config value into the env so it
	// survives restarts without needing DIGITORN_DAEMON_KEY set every launch.
	if cfg.Apps.Hub.DaemonKey != "" && os.Getenv("DIGITORN_DAEMON_KEY") == "" {
		_ = os.Setenv("DIGITORN_DAEMON_KEY", cfg.Apps.Hub.DaemonKey)
	}

	d := &Daemon{
		cfg:             cfg,
		logger:          logger,
		httpSrv:         httpSrv,
		rt:              siosrv,
		bus:             bus,
		eventBus:        evtBus,
		modules:         mods,
		gdb:             gdb,
		sessionStore:    store,
		sessionFlusher:  flusher,
		sessionPaths:    paths,
		envelopeBuilder: builder,
		bridge:          bridge,
		jwks:            jwks,
		jwtVerifier:     jwtVerifier,
		authValidator:   authVal,
		workerMgr:       wmgr,
		appCompiler:     appCompiler,
		appMgr:          am,
		provisioner:     provision.New(filepath.Join(filepath.Dir(cfg.Apps.Root), "tools"), nil, logger),
		userSkills:      userskills.NewStore(gdb),
		userSnippets:    usersnippets.NewStore(gdb),
		mcpOAuth:        mcpOAuth,
		managedMCP:      mcpSrvStore,
		creds:           credStore,
		credResolver:    credResolver,
		moduleSettings:  moduleSettings,
		secrets:         appSecrets,
		modelDefaults:   newModelDefaultsStore(gdb),
		mcpHub:          mcphub.NewClient(cfg.Apps.Hub.URL, cfg.Apps.Hub.Timeout, cfg.Apps.Hub.VerifySSL),
	}
	// Let the bridge resolve a session's window so a joining client gets the last
	// real context gauge on open (footer ctx used/window before any new turn).
	bridge.BrainFor = d.brainFor
	bridge.SessionWindowBrain = d.sessionWindowBrain
	bridge.PreWarmContext = d.preWarmContext
	// Embedded-preview iframes authenticate their socket with the `?t=` preview
	// token (no usable JWT), so they get live workspace_changes push instead of
	// polling. Same HMAC check as the preview/files routes.
	bridge.PreviewValidator = d.checkPreviewToken
	// Put provisioned requirement binaries (~/.digitorn/tools/bin) on the daemon's
	// PATH so every agent bash inherits them (buildEnv passes PATH through). Symlinks
	// added later resolve immediately, so this one prepend covers all future installs.
	if bd := d.provisioner.BinDir(); bd != "" {
		os.Setenv("PATH", bd+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	d.MountAPI()
	MountAuthProxy(httpSrv.Router(), cfg.Auth.ServiceURL)
	return d, nil
}

// buildSessionStore configures the on-disk session store + bus from config.
func buildSessionStore(cfg *config.Config, logger *slog.Logger) (*sessionstore.DiskFlusher, *sessionstore.Bus, sessionstore.Paths, error) {
	root := cfg.Sessions.Root
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, sessionstore.Paths{}, fmt.Errorf("resolve session root: %w", err)
		}
		root = filepath.Join(home, ".digitorn", "sessions")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, nil, sessionstore.Paths{}, fmt.Errorf("mkdir session root: %w", err)
	}

	paths := sessionstore.NewPaths(root)
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths:            paths,
		NumShards:        cfg.Sessions.NumShards,
		QueueCapPerShard: cfg.Sessions.QueueCapPerShard,
		BatchMax:         cfg.Sessions.BatchMax,
		FlushInterval:    cfg.Sessions.FlushInterval,
		Fsync:            cfg.Sessions.Fsync,
		FDCachePerShard:  cfg.Sessions.FDCachePerShard,
		PerSidQuotaPct:   cfg.Sessions.PerSidQuotaPct,
		OnWriteError: func(err error, sid string) {
			logger.Error("sessionstore: write failed",
				slog.String("sid", sid), slog.String("err", err.Error()))
		},
	})
	if err != nil {
		return nil, nil, sessionstore.Paths{}, err
	}

	bus, err := sessionstore.NewBus(sessionstore.BusConfig{
		Paths:                  paths,
		Flusher:                flusher,
		Logger:                 logger,
		SubscriberQueueSize:    cfg.Sessions.SubscriberQueueSize,
		SubscriberMaxSlowDrops: cfg.Sessions.SubscriberMaxSlowDrops,
		MaxStatesInMemory:      cfg.Sessions.MaxStatesInMemory,
		StateIdleEvictAfter:    cfg.Sessions.StateIdleEvictAfter,
		EvictionInterval:       cfg.Sessions.EvictionInterval,
	})
	if err != nil {
		return nil, nil, sessionstore.Paths{}, err
	}
	return flusher, bus, paths, nil
}

// manifestDirsFor returns the candidate module-manifest directories : the
// DIGITORN_MANIFESTS override, then `manifests/` next to the executable and the
// working directory.
func manifestDirsFor() []string {
	out := []string{}
	if v := os.Getenv("DIGITORN_MANIFESTS"); v != "" {
		out = append(out, v)
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "manifests"))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "manifests"))
	}
	return out
}

// busAdapter exposes the daemon's ServiceBus as a module.Bus.
type busAdapter struct{ bus ports.ServiceBus }

func (a busAdapter) Register(m domainmodule.Module) error { return a.bus.Register(m) }
func (a busAdapter) Unregister(id string) error           { return a.bus.Unregister(id) }

func (d *Daemon) Logger() *slog.Logger                           { return d.logger }
func (d *Daemon) ServiceBus() ports.ServiceBus                   { return d.bus }
func (d *Daemon) Realtime() ports.RealtimeServer                 { return d.rt }
func (d *Daemon) Modules() *module.Registry                      { return d.modules }
func (d *Daemon) DB() *gorm.DB                                   { return d.gdb }
func (d *Daemon) SessionStore() *sessionstore.Bus                { return d.sessionStore }
func (d *Daemon) SessionFlusher() *sessionstore.DiskFlusher      { return d.sessionFlusher }
func (d *Daemon) EnvelopeBuilder() *sessionstore.EnvelopeBuilder { return d.envelopeBuilder }
func (d *Daemon) SocketIOBridge() *SocketIOBridge                { return d.bridge }
func (d *Daemon) SessionPaths() sessionstore.Paths               { return d.sessionPaths }
func (d *Daemon) WorkerManager() *worker.Manager                 { return d.workerMgr }
func (d *Daemon) LLM() *llm.Client                               { return d.llmClient }
func (d *Daemon) Engine() runtime.Runner                         { return d.engine }
func (d *Daemon) EventBus() ports.EventBus                       { return d.eventBus }

// buildEngine wires the runtime once the LLM client is up. Skipped
// silently when no LLM worker is configured ; chat endpoints then
// degrade to "persist-only" (the user message lands, no assistant
// reply is generated).
func (d *Daemon) buildEngine() {
	if d.llmClient == nil {
		d.logger.Info("daemon: runtime engine disabled (no LLM client)")
		return
	}
	eng, err := runtime.New(d.appMgr, d.sessionStore, d.llmClient, d.logger)
	if err != nil {
		d.logger.Error("daemon: runtime engine build failed",
			slog.String("err", err.Error()))
		return
	}
	// Wire the 3-tier TurnPool from config. NewPool with zero caps =
	// unbounded ; the daemon must always provide explicit caps for
	// production safety. runtime.New already pre-installed an
	// unbounded pool ; we replace it.
	eng.Pool = turn.NewPool(turn.PoolConfig{
		GlobalCap:  d.cfg.Runtime.MaxConcurrentTurnsGlobal,
		PerAppCap:  d.cfg.Runtime.MaxConcurrentTurnsPerApp,
		PerUserCap: d.cfg.Runtime.MaxConcurrentTurnsPerUser,
	})

	// R-4 : enable per-token streaming. The engine emits
	// EventAssistantDelta per chunk when the LLM client supports
	// streaming (llm.Client does) ; the Socket.IO bridge forwards
	// those deltas to connected clients. Automatic fallback to the
	// synchronous path keeps non-streaming providers working.
	eng.Streaming = d.cfg.Runtime.Streaming
	// CTX-7 : non-blocking signal to the background Context Service to recount
	// the EXACT context size. The service itself is wired when the tokenizer
	// worker starts ; until then (or if disabled) this is a no-op and the
	// provider anchor keeps the gauge exact per turn.
	eng.ContextTouch = d.touchContext
	eng.ContextIncrement = d.touchContextIncrement
	// CTX-7 breakdown : the engine reports the assembled system prompt + tool
	// schemas at request-build so the background recount can attribute the
	// system / tools / messages buckets. Non-blocking (a map store + a Touch).
	eng.ContextRecordParts = d.recordContextParts
	// CTX-V : the runtime reads the freshest per-session context variable from
	// the Tracker (kept current by the Context Service notifications) for the
	// per-round compaction guard + hook pressure. Created once, fed by
	// onContextRecomputed ; empty Tracker → engine falls back to the snapshot.
	if d.contextTracker == nil {
		d.contextTracker = contextsvc.NewTracker()
	}
	eng.ContextLookup = d.contextTracker.Get
	eng.ModelWindowLookup = d.gatewayModelWindow
	eng.ContextRecordRatio = d.recordContextRatio
	// Per-tool timeout : bound a single tool dispatch so one slow/hung tool
	// can't eat the whole turn. Human-in-the-loop / sub-flow tools (ask_user,
	// run_parallel, …) are exempt inside the engine.
	eng.ToolTimeout = d.cfg.Runtime.ToolTimeout
	// Recover tool calls from models that emit them as text instead of native
	// tool_calls (DeepSeek, Hermes/Qwen, …). The format knowledge lives in the
	// standalone internal/llm/toolcall library ; this is the only wiring.
	eng.ResponseNormalizer = llm.NormalizeTextToolCalls
	// Wire the SG-5 approval registry so the resolveApproval HTTP
	// handler can signal goroutines awaiting on a NeedsApproval
	// gate decision. Held on the Daemon so the handler can reach it.
	d.approvalRegistry = approval.NewRegistry()
	eng.ApprovalRegistry = d.approvalRegistry

	// SG-4 : wire the runtime policy evaluator so the documented gates
	// run on every LLM-emitted tool call. Without this the engine skips
	// enforcement entirely — capabilities.deny is then honoured only by
	// the schema filter (SG-3) and capabilities.approve never fires. The
	// ToolSpecLookup resolves RiskLevel + permissions from the same
	// module registry the dispatcher executes against, so gates 2/3 see
	// the real spec. Meta-tools and system modules bypass before any
	// lookup, so this never blocks context_builder primitives.
	mcpCat := newMCPCatalog(d.modules, d.appMgr, d.mcpOAuth)
	d.mcpCatalog = mcpCat // shared with the OAuth handlers (server-config lookup)
	piecesCat := newPiecesCatalog(d.modules, d.appMgr)
	d.piecesCatalog = piecesCat
	if d.mcpOAuth != nil {
		d.mcpOAuth.SetServerAuthLookup(d.mcpServerAuthLookup)
		d.mcpOAuth.SetServerURLLookup(d.mcpServerURLLookup)
		d.mcpOAuth.SetRedirectBase(d.previewBaseURL())
		d.mcpOAuth.SetPieceRedirectURL(d.cfg.OAuth.PieceRedirectURL)
	}
	eng.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: registryToolSpecs{Registry: d.modules, MCP: mcpCat, Pieces: piecesCat},
	}

	// WD : per-session workdir confinement. The engine attaches the resolved
	// PathPolicy to the dispatch ctx each turn ; the chokepoint then confines
	// every path-typed tool arg to the session's workdir. nil-safe : sessions
	// with no workdir simply get no policy.
	eng.PathPolicies = sessionPathPolicies{store: d.sessionStore, apps: d.appMgr}

	// MW-C2 : resolve `custom` app-middleware entries to an out-of-process
	// gRPC plugin. A custom entry names a worker `module` hosted in a worker
	// `kind` pool ; the Proxy forwards Before/After over the same generic
	// module service the dispatcher uses, so no new transport is needed. The
	// worker pool must already host the module's before/after tools. Without
	// this seam a `custom` entry is skipped with a warning (registry.Build).
	eng.MiddlewareCustomFactory = func(name string, cfg map[string]any) (ports.AppMiddleware, error) {
		moduleID, _ := cfg["module"].(string)
		kind, _ := cfg["kind"].(string)
		if moduleID == "" || kind == "" {
			return nil, fmt.Errorf("custom middleware requires `module` and `kind`")
		}
		var timeout time.Duration
		switch v := cfg["timeout"].(type) {
		case int:
			timeout = time.Duration(v) * time.Second
		case int64:
			timeout = time.Duration(v) * time.Second
		case float64:
			timeout = time.Duration(v * float64(time.Second))
		}
		failOpen := true
		if b, ok := cfg["fail_open"].(bool); ok {
			failOpen = b
		}
		return middlewareplugin.New(middlewareplugin.Options{
			Name:     "custom:" + moduleID,
			ModuleID: moduleID,
			Kind:     worker.Kind(kind),
			Picker:   d.workerMgr,
			Timeout:  timeout,
			FailOpen: failOpen,
			Logger:   d.logger,
		})
	}

	// CB-6 + RT-3 : wire the context_builder and the dispatcher.
	//
	// The wiring.Builder is the single source of truth for the
	// per-agent ToolIndex (CB-1..5) AND the IndexLookup that the
	// MetaDispatcher uses to route meta-tool calls. We hand it the
	// production action source : registryActions intersects the
	// app's declared modules with the in-process module registry.
	//
	// The dispatch chain :
	//
	//     LLM -> MetaDispatcher (meta-tools handled locally)
	//                |
	//                v
	//            BusAdapter (domain tools -> module via servicebus)
	//
	// The MetaDispatcher's IndexLookup hits the same cache the
	// engine populated via Context.BuildFor at the start of the
	// turn, guaranteeing the meta-tools see the same tool universe
	// the LLM was offered. RT-3 isolation contract enforced.
	actions := registryActions{Registry: d.modules, Apps: d.appMgr, MCP: mcpCat, Pieces: piecesCat}
	contextBuilder := wiring.New(actions)
	// CE-7 : enable semantic search when an embeddings worker is
	// up. Falls back silently to keyword-only when nil per the
	// doc-defined graceful-degrade contract.
	if d.embeddingsClient != nil {
		contextBuilder = contextBuilder.WithEmbeddings(d.embeddingsClient)
	}
	// CBF : module-driven prompt contributions (get_prompt_sections /
	// get_dynamic_tool_prompts). Authorization-gated by the wiring layer to
	// the agent's authorized modules, so a module's prompt sections never
	// leak to an unauthorized agent.
	contextBuilder = contextBuilder.WithContributors(registryContributors{Registry: d.modules, Pieces: d.piecesCatalog})
	eng.Context = contextBuilder
	d.promptBuilder = contextBuilder

	busAdapter := dispatch.NewBusAdapter(d.bus)
	// Per-app module config delivery : resolve tools.modules.<id>.config for
	// each call so a shared (in-proc or worker) module reads its app-specific
	// configuration. The worker proxy forwards it across the boundary.
	busAdapter.ModuleConfigs = appModuleConfigSource{apps: d.appMgr, deltas: d.moduleSettings, secrets: d.ensureSecretStore()}
	// Wire the EventBus so modules that implement EventEmitter can publish events.
	busAdapter.EventBus = d.eventBus
	busAdapter.AppsRoot = d.cfg.Apps.Root
	// In-proc embeddings/rerank : in-proc modules (filesystem code-grep) reach
	// the embeddings client the same way worker modules reach the gateway. Lazy
	// (reads d.embeddingsClient at call time — wired later in startWorkers).
	busAdapter.Embedder = daemonEmbedder{d: d}
	busAdapter.Reranker = daemonReranker{d: d}
	// MW-C3 : wire the per-app tool-call middleware onion. The source resolves
	// (and caches) one pipeline per (app, module) from the app's
	// tools.modules.<id>.middleware config, so retry / timeout /
	// circuit_breaker / audit / dedup / semantic_cache / auto_heal /
	// cross_context / budget wrap every domain tool call — in-process or
	// worker-hosted — with per-session isolation for the stateful layers.
	if busAdapter != nil {
		busAdapter.Pipelines = newToolPipelineSource(
			d.appMgr, d.embeddingsClient, newToolResolver(d.modules), d.logger)
	}

	// FT-5 : content-addressed BlobStore, wired on BOTH sides so multimodal
	// tool output works end to end. busAdapter.Blobs (Put) stores binary tool
	// output — an image read of a PNG — and eng.Blobs (Get) lets the LLM
	// multipart adapter resolve those blobs back into vision content the model
	// actually sees. Same *Store instance ; lives under the sessions root.
	blobStore := blobstore.New(filepath.Join(d.cfg.Sessions.Root, "blobs"))
	if busAdapter != nil {
		busAdapter.Blobs = blobStore
	}
	eng.Blobs = blobStore
	d.blobStore = blobStore // expose for the inbound upload endpoint (any client)

	// Off-path office→PDF preview converter (bounded; no-op when LibreOffice is
	// absent — the web client then falls back to the pure-JS pptx viewer).
	d.officeConverter = newOfficeConverter(filepath.Join(d.cfg.Sessions.Root, "pdfcache"), d.logger)

	// Per-user BYOK key injection: in BYOK mode the engine prefers a key the
	// user stored in their vault (O(1) cached lookup) over the bundle key.
	// Guard the assignment so a nil *Resolver never becomes a non-nil interface.
	if d.credResolver != nil {
		eng.CredResolver = d.credResolver
	}
	eng.AppSecrets = daemonAppSecrets{store: d.ensureSecretStore()}

	// Image/video generation: image/video-kind agents POST to the gateway's
	// dedicated /v1/{images,videos}/generations endpoints (NOT chat-completions).
	if gw := strings.TrimSpace(d.cfg.Workers.LLM.GatewayURL); gw != "" {
		eng.MediaGen = mediagen.New(gw)
	}

	// Brique 4 (live preview) : the filesystem module signals every write/edit
	// through this notifier, which debounces per session and pushes the agent's
	// pending changes straight to the session's realtime room — off the hot path,
	// ephemeral (never persisted). nil when the realtime stack is absent (tests).
	if busAdapter != nil {
		if wl := d.newWorkspaceLive(); wl != nil {
			busAdapter.FileChangeNotifier = wl
			d.workspaceLive = wl // reused by the REST file-save route for the live git refresh
		}
	}

	// P-1f : BackgroundManager runs goroutines for context_builder.
	// background_run. Sharing the dispatcher means launched tasks
	// go through the same security gates and audit row pipeline as
	// foreground calls.
	bgMgr := background.New()
	bgMgr.AttachDispatcher(busAdapter)
	// Publish background-task lifecycle events on the session bus so the
	// client sees tasks appear / finish / fail in real time (the Socket.IO
	// bridge forwards every session-scoped event to the session room).
	bgMgr.AttachSink(d.sessionStore)
	bgMgr.WorkspaceTouched = func(sessionID string) {
		if d.workspaceLive == nil || d.sessionStore == nil {
			return
		}
		st, err := d.sessionStore.State(sessionID)
		if err != nil || st == nil {
			return
		}
		st.RLock()
		wd := st.Workdir
		st.RUnlock()
		if wd != "" {
			d.workspaceLive.FileChanged(sessionID, wd)
		}
	}
	// Desktop / local only: when the agent starts a dev server (npm run dev …),
	// point the preview straight at its localhost URL. Reachable only when the
	// daemon shares the client's machine, so the prod cloud daemon (channel
	// "server") leaves detection off (nil callback = no watcher goroutines).
	if d.cfg.Apps.Channel != "server" {
		bgMgr.DevServerDetected = func(sessionID, rawURL string) {
			if d.rt == nil {
				return
			}
			// Never mistake the daemon's own address for the app's dev server.
			if u, err := neturl.Parse(rawURL); err == nil && u.Port() == strconv.Itoa(d.cfg.Server.Port) {
				return
			}
			_ = d.rt.Emit(context.Background(), bridgeNamespace, "session:"+sessionID, "web_preview:attached", map[string]any{
				"session_id": sessionID,
				"name":       "default",
				"url":        rawURL,
				"type":       "devserver",
			})
		}
	}
	d.background = bgMgr

	// Single entry point for starting a turn from any source (user message
	// or proactive wake). Guarantees at most one turn per session at a time,
	// coalescing concurrent triggers. The background manager wakes through
	// it so a finished task notifies the agent immediately without colliding
	// with an in-flight turn.
	// Idle safety window for a turn (resets on progress — see sessionRunner).
	// Configurable; falls back to the built-in default. Must stay above the
	// per-tool timeout so a single slow tool can never be read as a stall.
	idleWindow := d.cfg.Runtime.TurnIdleTimeout
	if idleWindow <= 0 {
		idleWindow = turnSafetyCutoff
	}
	if tt := d.cfg.Runtime.ToolTimeout; tt > 0 && tt >= idleWindow {
		d.logger.Warn("daemon: tool_timeout >= turn_idle_timeout; a single slow tool may trip the turn watchdog",
			slog.Duration("tool_timeout", tt),
			slog.Duration("turn_idle_timeout", idleWindow))
	}
	d.sessionRunner = newSessionRunner(func(ctx context.Context, in runtime.TurnInput) error {
		_, err := eng.Run(ctx, in)
		return err
	}, idleWindow, d.logger)
	bgMgr.AttachWaker(d.sessionRunner)
	eng.BackgroundNotifications = &bgNotifierAdapter{mgr: bgMgr}

	// MA : the multi-agent orchestrator. The engine is the sub-agent runner
	// (RunSubAgent) ; the session store is the durable lifecycle sink so the
	// agent tree replays for client resync. The per-call LLM semaphore is the
	// real throttle for a swarm of sub-agents — set it when configured ; the
	// sub-agent turn-pool stays unbounded so nested delegation never deadlocks.
	agentMgr := agent.New(d.logger)
	agentMgr.AttachRunner(eng)
	agentMgr.AttachSink(d.sessionStore)
	agentKV := agent.NewKV()
	agentMgr.MaxDepth = d.cfg.Runtime.MaxAgentDepth
	agentMgr.MaxAgentsPerRoot = d.cfg.Runtime.MaxAgentsPerSession
	d.agents = agentMgr
	if n := d.cfg.Runtime.MaxConcurrentLLMCalls; n > 0 {
		eng.LLMSem = runtime.NewPrioritySemaphore(n)
	}

	// P-1d : SkillLoader resolves use_skill /commands. Two layers in one
	// registry — the caller's own authored skills (DB-backed, per user × app)
	// take precedence, then the app's bundled dev.skills[] markdown. The user
	// layer is adapted here so the skills package stays persistence-free.
	userSkills := d.userSkills
	skillLoader := skills.NewLayered(
		skills.UserLoaderFunc(func(ctx context.Context, appID, userID, command string) (meta.SkillEntry, bool, error) {
			sk, found, err := userSkills.GetByName(ctx, userID, appID, command)
			if err != nil || !found {
				return meta.SkillEntry{}, found, err
			}
			return meta.SkillEntry{
				Command:     "/" + sk.Name,
				Description: sk.Description,
				Content:     sk.Instructions,
			}, true, nil
		}),
		skills.New(d.appMgr),
	)

	// User-driven path : a message carrying a `/command` skill makes the engine
	// inject that skill's instructions as a forced directive (injectSkillDirective).
	// Same layered loader as the agent's use_skill tool, adapted to the runtime's
	// SkillContent shape (runtime can't import meta).
	eng.SkillLoader = skillContentAdapter{inner: skillLoader}

	eng.Dispatcher = &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex {
			return contextBuilder.IndexFor(appID, agentID)
		},
		Inner:       busAdapter,
		Background:  bgMgr,
		SkillLoader: skillLoader,
		// MA : the `agent` delegation tool, coordinator-gated.
		Agents:            agentManagerAdapter{m: agentMgr},
		KV:                agentKV,
		CoordinatorLookup: newCoordinatorLookup(d.appMgr),
		// MEM : the working-memory tools (set_goal / remember / task_create /
		// task_update). The engine is the single durable home — one event per
		// mutation, no side store.
		Memory: eng,
		// Per-child run_parallel progress → the client sees each action finish
		// live (the bridge forwards every session event). Best-effort + transient
		// to the agent (EventToolProgress isn't projected into its history), so
		// the combined barrier result it receives is unchanged.
		Progress: func(ctx context.Context, ev sessionstore.Event) {
			_, _ = d.sessionStore.AppendDurable(ctx, ev)
		},
		// SG-4 chokepoint : the engine gates every domain sub-tool
		// reached via execute_tool / run_parallel / background_run, so
		// capabilities.deny / approve can't be bypassed by the meta
		// indirection. Direct calls are gated by the engine top-level ;
		// each real sub-tool is therefore evaluated exactly once.
		Gate: eng,
		// Surface recovered sub-tool panics (run_parallel) with their
		// stack trace instead of losing them silently.
		Logger: d.logger,
		// ask_user : the synchronous human-input primitive, backed by
		// the SG-5 approval registry + session bus (see askuser.go).
		// Offered to an agent only when the app grants
		// context_builder.ask_user (relevance-gated in the wiring).
		AskUser: &askUserBridge{store: d.sessionStore, reg: d.approvalRegistry, logger: d.logger},
		// AppCaller (call_app) wired in a future sprint ; nil here
		// means the LLM gets a clean "not wired" message until then.
	}

	// RT-4 : wire the production hook engine source. Without this
	// every runtime.hooks[] block in app YAMLs is parsed by the
	// compiler but never executed by the runtime — the
	// documented "JVM-equivalent" stays dormant. The source builds
	// one *hooks.Engine per app on demand, caches it so cooldown
	// and max_fires counters survive across turns, and routes
	// module_action targets through the same dispatcher the LLM
	// uses (same security gates, same audit row, same
	// canonicalisation).
	compactor := newContextCompactor(d.sessionStore, d.appMgr, d.llmClient, d.logger)
	compactor.touch = d.touchContext
	compactor.touchSync = d.touchContextSync
	compactor.credResolver = d.credResolver
	d.compactor = compactor
	// Background summary maintainer: keeps a high-fidelity LLM summary prepared
	// off the turn loop so compaction is zero-latency (instant apply, no blocking
	// LLM call). Uses delta-mode by default: only new messages since the last
	// coverage are sent to the LLM, making it 5-10x cheaper for long sessions.
	// Disable with DIGITORN_CONTEXT_BG_SUMMARY=0 to fall back to inline summarize.
	if !contextBGSummaryDisabled() {
		workers := 8
		sm := newSummaryMaintainer(d.sessionStore, compactor, d.llmClient, d.logger)
		sm.Start(workers)
		d.summaryMaintainer = sm
		compactor.nonBlocking = true // apply prepared summary instantly; else truncate
		compactor.prepare = sm.Touch
		eng.PrepareSummary = sm.Touch
		eng.MicroCompactView = true
		d.logger.Info("context: background summary maintainer started",
			slog.Int("workers", workers))
	}
	eng.Compactor = compactor // mid-turn emergency recovery on context overflow
	eng.Hooks = newHookSource(d.appMgr, hooks.ActionDeps{
		Logger:    d.logger,
		Sink:      d.sessionStore,
		Caller:    dispatchCaller{d: eng.Dispatcher},
		Compactor: compactor,
	})

	// Auto-promote: a FOREGROUND bash run still going after the threshold is moved
	// to a managed background task instead of blocking the turn or being killed.
	// Wrap the dispatcher AFTER hooks captured the raw one, so only the LLM's tool
	// calls auto-promote; hook-driven and meta sub-tool dispatches run directly.
	eng.Dispatcher = background.NewPromotingDispatcher(eng.Dispatcher, bgMgr, background.DefaultPromoteThreshold)

	d.engine = eng
	d.lifecycle = eng // *runtime.Engine exposes FireLifecycle for session_end / pre_compact
	d.logger.Info("daemon: runtime engine ready",
		slog.Int("turn_pool_global", d.cfg.Runtime.MaxConcurrentTurnsGlobal),
		slog.Int("turn_pool_per_app", d.cfg.Runtime.MaxConcurrentTurnsPerApp),
		slog.Int("turn_pool_per_user", d.cfg.Runtime.MaxConcurrentTurnsPerUser),
		slog.Bool("dispatcher_wired", busAdapter != nil),
		slog.Bool("hooks_wired", eng.Hooks != nil),
	)
}

// bgNotifierAdapter bridges runtime.BackgroundNotifier (the
// engine-facing contract) to *background.Manager. The runtime
// can't import background directly (would invert the dependency
// order), so we expose the adapter here.
type bgNotifierAdapter struct{ mgr *background.Manager }

func (a *bgNotifierAdapter) DrainNotifications(sessionID string) []runtime.BackgroundNotification {
	pending := a.mgr.DrainNotifications(sessionID)
	if len(pending) == 0 {
		return nil
	}
	out := make([]runtime.BackgroundNotification, len(pending))
	for i, p := range pending {
		out[i] = p
	}
	return out
}

// HTTPRouter returns the Chi router for mounting application routes.
func (d *Daemon) HTTPRouter() interface {
	Handle(pattern string, h nethttp.Handler)
} {
	return d.httpSrv.Router()
}

// Start starts every registered module and then the HTTP/Socket.IO listener.
// Blocks until ctx is canceled or the HTTP server returns an error.
func (d *Daemon) startPiecesTokenRefresh(ctx context.Context) {
	pm := d.piecesModule()
	if pm == nil || pm.PiecesStore() == nil {
		return
	}
	store := pm.PiecesStore()
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		refreshed, failed := store.RefreshExpiring(ctx, 30*time.Minute)
		if refreshed > 0 || failed > 0 {
			d.logger.Info("pieces: proactive token refresh", slog.Int("refreshed", refreshed), slog.Int("failed", failed))
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshed, failed := store.RefreshExpiring(ctx, 30*time.Minute)
				if refreshed > 0 || failed > 0 {
					d.logger.Info("pieces: proactive token refresh", slog.Int("refreshed", refreshed), slog.Int("failed", failed))
				}
			}
		}
	}()
}

func (d *Daemon) Start(ctx context.Context) error {
	d.logger.Info("daemon: starting",
		slog.String("addr", d.httpSrv.HTTPServer().Addr),
		slog.String("db_driver", d.cfg.Database.Driver),
		slog.String("sessions_root", d.cfg.Sessions.Root),
	)

	if err := d.sessionFlusher.Start(); err != nil {
		return fmt.Errorf("daemon: session flusher: %w", err)
	}
	if err := d.sessionStore.Start(ctx); err != nil {
		return fmt.Errorf("daemon: session bus: %w", err)
	}
	if d.jwks != nil {
		if err := d.jwks.Start(ctx); err != nil {
			return fmt.Errorf("daemon: jwks: %w", err)
		}
	}
	if err := d.workerMgr.Start(); err != nil {
		return fmt.Errorf("daemon: worker manager: %w", err)
	}
	d.startWorkers(ctx)
	d.buildEngine()
	// Subscribe to EventBus events and buffer them for the background service
	// primitives adapter to poll via GET /api/events/recent.
	d.subscribeToEventBus()
	if d.agents != nil {
		d.agents.Start(ctx) // background reaper for terminal sub-agents / empty roots
	}

	// Spawn every configured worker pool BEFORE starting in-proc
	// modules, so we know which modules are workerised and can skip
	// their in-proc instantiation. Each pool's modules become
	// ProxyModule entries in the servicebus.
	workerisedIDs := d.startWorkerPools(ctx)

	// Load every enabled app from the DB into the snapshot before we
	// accept HTTP traffic. Broken apps are logged + skipped, never
	// fatal — the daemon serves whatever subset boots successfully.
	if err := d.appMgr.Bootstrap(ctx); err != nil {
		d.logger.Error("daemon: app manager bootstrap", slog.String("err", err.Error()))
	}
	if err := d.bridge.Start(ctx); err != nil {
		return fmt.Errorf("daemon: bridge: %w", err)
	}
	d.startPiecesTokenRefresh(ctx)
	d.startBackgroundSupervisor(ctx)
	d.startVoiceSupervisor(ctx)
	// Provision server-channel apps hosted on the hub (heavy web apps kept out
	// of the binary) asynchronously — boot never waits on hub/network.
	if d.appMgr != nil {
		go d.appMgr.ReconcileHubApps(ctx)
	}

	// Start in-proc modules EXCEPT those served by a worker pool —
	// their daemon-side instance stays dormant so we don't double-
	// run them (once in-proc, once in a worker subprocess).
	if errs := d.modules.StartExcept(ctx, workerisedIDs); len(errs) > 0 {
		for _, e := range errs {
			d.logger.Error("daemon: module start error", slog.String("err", e.Error()))
		}
	}

	httpErr := make(chan error, 1)
	go func() {
		if err := d.httpSrv.Start(); err != nil {
			httpErr <- err
		}
		close(httpErr)
	}()

	select {
	case <-ctx.Done():
		return d.Shutdown()
	case err := <-httpErr:
		if err != nil {
			return fmt.Errorf("daemon: http: %w", err)
		}
		return nil
	}
}

// Shutdown stops every component gracefully.
func (d *Daemon) Shutdown() error {
	var shutdownErr error
	d.once.Do(func() {
		d.logger.Info("daemon: shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Server.ShutdownTimeout)
		defer cancel()

		if err := d.httpSrv.Shutdown(ctx); err != nil {
			d.logger.Warn("daemon: http shutdown error", slog.String("err", err.Error()))
			shutdownErr = err
		}
		if err := d.rt.Close(ctx); err != nil {
			d.logger.Warn("daemon: socketio close error", slog.String("err", err.Error()))
		}
		if errs := d.modules.StopAll(ctx); len(errs) > 0 {
			for _, e := range errs {
				d.logger.Warn("daemon: module stop error", slog.String("err", e.Error()))
			}
		}
		if d.bridge != nil {
			_ = d.bridge.Stop(ctx)
		}
		if bg := d.contextBG.Load(); bg != nil {
			bg.Stop() // drain in-flight EXACT recomputes
		}
		if d.summaryMaintainer != nil {
			d.summaryMaintainer.Stop() // drain in-flight background summaries (CTX-8)
		}
		if d.agents != nil {
			d.agents.Stop() // end the sub-agent reaper
		}
		if d.workerMgr != nil {
			if err := d.workerMgr.Stop(ctx); err != nil {
				d.logger.Warn("daemon: worker manager stop error", slog.String("err", err.Error()))
			}
		}
		// Gateway after the workers that dial it are gone.
		if d.serviceGateway != nil {
			d.serviceGateway.Stop()
		}
		if d.eventBus != nil {
			_ = d.eventBus.Close(ctx)
		}
		if d.jwks != nil {
			_ = d.jwks.Stop(ctx)
		}
		if err := d.sessionStore.Stop(ctx); err != nil {
			d.logger.Warn("daemon: session bus stop error", slog.String("err", err.Error()))
		}
		if err := d.sessionFlusher.Stop(ctx); err != nil {
			d.logger.Warn("daemon: session flusher stop error", slog.String("err", err.Error()))
		}
		if sqlDB, err := d.gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return shutdownErr
}

func newLogger(cfg config.Logging) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	return slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: level, TimeFormat: time.RFC3339}))
}
