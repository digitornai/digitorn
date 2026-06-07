// Package config loads daemon configuration from YAML files and environment
// variables (env vars override file values). Env var format:
//
//	DIGITORN_<SECTION>__<KEY>     (double underscore as nested delimiter)
//
// Example:
//
//	DIGITORN_SERVER__PORT=9000
//	DIGITORN_DATABASE__DSN=postgres://...
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the root daemon configuration.
type Config struct {
	Server        Server        `koanf:"server"`
	SocketIO      SocketIO      `koanf:"socketio"`
	Database      Database      `koanf:"database"`
	Sessions      Sessions      `koanf:"sessions"`
	Apps          Apps          `koanf:"apps"`
	Auth          Auth          `koanf:"auth"`
	Modules       Modules       `koanf:"modules"`
	Runtime       Runtime       `koanf:"runtime"`
	Workers       Workers       `koanf:"workers"`
	Logging       Logging       `koanf:"logging"`
	LLM           LLM           `koanf:"llm"`
	Observability Observability `koanf:"observability"`
}

// Apps holds the install location for apps and the hub client settings.
//
// Each installed app lives in {Root}/{app_id}/ with the full source dir
// copied verbatim (app.yaml + prompts/ + skills/ + behavior/ + assets/ +
// web/ + anything else), plus a generated app.dgc compiled bundle next
// to it. The runtime loads app.dgc directly (JVM-style) — never the
// raw YAML at boot. Reload regenerates app.dgc from source.
type Apps struct {
	Root string  `koanf:"root"`
	Hub  AppsHub `koanf:"hub"`
}

// AppsHub configures the digitorn app marketplace HTTP client. The hub
// serves source-only tar.gz archives ; the daemon compiles them locally
// at install time. Auth is per-request : the user's JWT is forwarded
// from the install REST call as Bearer header.
type AppsHub struct {
	URL             string        `koanf:"url"`
	Timeout         time.Duration `koanf:"timeout"`
	VerifySSL       bool          `koanf:"verify_ssl"`
	MaxArchiveBytes int64         `koanf:"max_archive_bytes"`
}

// Workers groups subprocess worker pool configurations.
//
//   - LLM is the legacy hard-wired pool (kept as-is for backward
//     compatibility ; one day it migrates to Pools[] like the others).
//   - Pools[] is the generic config-driven list : each entry spawns N
//     subprocesses of cmd/digitorn-worker, each hosting the listed
//     modules. The daemon registers a ProxyModule per (pool, module)
//     pair so the runtime sees the modules as if they were in-proc.
type Workers struct {
	LLM        WorkerLLM        `koanf:"llm"`
	Embeddings WorkerEmbeddings `koanf:"embeddings"`
	Tokenizer  WorkerTokenizer  `koanf:"tokenizer"`
	Pools      []WorkerPool     `koanf:"pools"`
}

// WorkerTokenizer holds the tokenizer worker subprocess configuration
// (CTX-7 context occupancy refinement). Count=0 disables the worker ; the
// daemon then keeps the provider usage anchor as the occupancy gauge — exact at
// every turn boundary — so disabling the worker is fully graceful, never a
// correctness loss. The worker only refines the between-anchor delta.
type WorkerTokenizer struct {
	Count         int           `koanf:"count"`          // 0 = disabled
	BinaryPath    string        `koanf:"binary_path"`    // empty = auto-discover
	StartTimeout  time.Duration `koanf:"start_timeout"`  // default (manager) — fast: no model load
	StopTimeout   time.Duration `koanf:"stop_timeout"`   // default 10s
	BackoffMin    time.Duration `koanf:"backoff_min"`    // default 500ms
	BackoffMax    time.Duration `koanf:"backoff_max"`    // default 30s
	MaxFailures   int           `koanf:"max_failures"`   // default 5
	HealthEvery   time.Duration `koanf:"health_every"`   // default 5s
	ClientTimeout time.Duration `koanf:"client_timeout"` // default 5s
}

// WorkerEmbeddings holds the embeddings worker subprocess configuration
// (semantic search for context_builder). Count=0 disables the
// worker ; the daemon then falls back to keyword-only search per
// docs-site/language/04-tools.md "Semantic search" graceful degrade.
type WorkerEmbeddings struct {
	Count         int           `koanf:"count"`          // 0 = disabled
	BinaryPath    string        `koanf:"binary_path"`    // empty = auto-discover
	Backend       string        `koanf:"backend"`        // "onnx" | "deterministic" | "" (auto)
	ModelDir      string        `koanf:"model_dir"`      // override model cache dir
	Quantized     bool          `koanf:"quantized"`      // use the int8 model (model_quantized.onnx, ~4x smaller/faster)
	Device        string        `koanf:"device"`         // "" / auto | cpu | cuda | directml | coreml (GPU needs a matching onnxruntime build)
	StartTimeout  time.Duration `koanf:"start_timeout"`  // default 30s (model load can be slow)
	StopTimeout   time.Duration `koanf:"stop_timeout"`   // default 10s
	BackoffMin    time.Duration `koanf:"backoff_min"`    // default 500ms
	BackoffMax    time.Duration `koanf:"backoff_max"`    // default 30s
	MaxFailures   int           `koanf:"max_failures"`   // default 5
	HealthEvery   time.Duration `koanf:"health_every"`   // default 5s
	ClientTimeout time.Duration `koanf:"client_timeout"` // default 10s
}

// WorkerPool declares one subprocess pool that hosts a set of modules.
// The daemon spawns Count instances of Binary (default :
// digitorn-worker), each booted with DIGITORN_WORKER_MODULES=Modules
// and any per-module config in Env.
type WorkerPool struct {
	// ID is the pool identifier — used as worker.Kind on the wire
	// and shown in /diagnostics. Must be unique across Pools.
	ID string `koanf:"id"`

	// Modules is the list of module IDs this pool hosts. Each will
	// be registered in the daemon's servicebus as a ProxyModule
	// instead of being instantiated in-process.
	Modules []string `koanf:"modules"`

	// Count is how many subprocesses to spawn. Default 1.
	Count int `koanf:"count"`

	// BinaryPath overrides the worker binary location. Empty =
	// auto-discover digitorn-worker via resolveWorkerBinary().
	BinaryPath string `koanf:"binary_path"`

	// Env is extra env vars passed to every subprocess. Use
	// DIGITORN_MODULE_<UPPER>_CONFIG to seed per-module config.
	Env map[string]string `koanf:"env"`

	// StartTimeout caps the time to wait for first SERVING.
	// Default 15s.
	StartTimeout time.Duration `koanf:"start_timeout"`

	// StopTimeout caps graceful shutdown. Default 10s.
	StopTimeout time.Duration `koanf:"stop_timeout"`

	// BackoffMin / BackoffMax frame restart backoff. Defaults
	// 500ms / 30s respectively.
	BackoffMin time.Duration `koanf:"backoff_min"`
	BackoffMax time.Duration `koanf:"backoff_max"`

	// MaxFailures stops the restart loop after N consecutive
	// failures. Default 5 ; 0 = unlimited.
	MaxFailures int `koanf:"max_failures"`

	// InvokeTimeout caps a single ProxyModule.Invoke RPC. Default
	// 60s (long enough for heavy LSP / MCP calls).
	InvokeTimeout time.Duration `koanf:"invoke_timeout"`

	// Transport selects the daemon↔worker gRPC transport. "unix" binds an
	// AF_UNIX socket (~2.8× lower round-trip latency than TCP loopback —
	// worth it for pools hosting fast tools like shell/filesystem).
	// Anything else (default) uses TCP loopback. An explicit
	// DIGITORN_WORKER_BIND in Env always wins.
	Transport string `koanf:"transport"`
}

// WorkerLLM holds the LLM worker subprocess configuration. Count=0 disables
// the worker entirely (useful for tests and for daemons that only serve the
// REST API without LLM calls).
type WorkerLLM struct {
	Count         int           `koanf:"count"`          // 0 = disabled (default 1)
	BinaryPath    string        `koanf:"binary_path"`    // empty = auto-discover
	GatewayURL    string        `koanf:"gateway_url"`    // digitorn LLM gateway base URL
	Concurrency   int           `koanf:"concurrency"`    // per-provider concurrency (default 256)
	BufferSize    int           `koanf:"buffer_size"`    // per-provider buffer (default 16384)
	StartTimeout  time.Duration `koanf:"start_timeout"`  // default 15s
	StopTimeout   time.Duration `koanf:"stop_timeout"`   // default 10s
	BackoffMin    time.Duration `koanf:"backoff_min"`    // default 500ms
	BackoffMax    time.Duration `koanf:"backoff_max"`    // default 30s
	MaxFailures   int           `koanf:"max_failures"`   // default 5 (0=unlimited)
	HealthEvery   time.Duration `koanf:"health_every"`   // default 5s
	ClientRetries int           `koanf:"client_retries"` // default 1
	ClientTimeout time.Duration `koanf:"client_timeout"` // default 60s

	// Per-provider concurrency / buffer overrides. Each maps a normalised
	// (lower-case) provider name to a positive int. Empty / missing key
	// → fall back to Concurrency / BufferSize.
	// YAML example:
	//   per_provider_concurrency: {anthropic: 1024, deepseek: 64}
	//   per_provider_buffer:      {anthropic: 32768, deepseek: 1024}
	PerProviderConcurrency map[string]int `koanf:"per_provider_concurrency"`
	PerProviderBufferSize  map[string]int `koanf:"per_provider_buffer"`
}

// Auth holds the JWT/JWKS validation config. The daemon does NOT
// implement login/refresh/logout — those live on the external auth
// service. The daemon only validates tokens issued by that service.
type Auth struct {
	Enabled             bool          `koanf:"enabled"`
	ServiceURL          string        `koanf:"service_url"`
	Issuer              string        `koanf:"issuer"`
	Audience            string        `koanf:"audience"`
	JWKSURL             string        `koanf:"jwks_url"`
	UserIDClaim         string        `koanf:"user_id_claim"`
	JWKSRefreshInterval time.Duration `koanf:"jwks_refresh_interval"`
	JWKSFailureBackoff  time.Duration `koanf:"jwks_failure_backoff"`
	JWTLeeway           time.Duration `koanf:"jwt_leeway"`
	DevMode             bool          `koanf:"dev_mode"`
}

// Sessions holds the on-disk session store configuration. Persistence is
// JSONL + snapshot.json under Root. Postgres is NEVER used for session
// runtime data — only metadata (apps, credentials, audit, module state).
type Sessions struct {
	Root                   string        `koanf:"root"`
	NumShards              int           `koanf:"num_shards"`
	QueueCapPerShard       int           `koanf:"queue_cap_per_shard"`
	BatchMax               int           `koanf:"batch_max"`
	FlushInterval          time.Duration `koanf:"flush_interval"`
	FDCachePerShard        int           `koanf:"fd_cache_per_shard"`
	PerSidQuotaPct         int           `koanf:"per_sid_quota_pct"`
	Fsync                  bool          `koanf:"fsync"`
	MaxStatesInMemory      int           `koanf:"max_states_in_memory"`
	StateIdleEvictAfter    time.Duration `koanf:"state_idle_evict_after"`
	EvictionInterval       time.Duration `koanf:"eviction_interval"`
	SubscriberQueueSize    int           `koanf:"subscriber_queue_size"`
	SubscriberMaxSlowDrops uint64        `koanf:"subscriber_max_slow_drops"`
	InstanceID             string        `koanf:"instance_id"`
	Capabilities           []string      `koanf:"capabilities"`
}

// Server holds HTTP server settings.
type Server struct {
	Host            string        `koanf:"host"`
	Port            int           `koanf:"port"`
	ReadTimeout     time.Duration `koanf:"read_timeout"`
	WriteTimeout    time.Duration `koanf:"write_timeout"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
	CORSOrigins     []string      `koanf:"cors_origins"`
	AuthEnabled     bool          `koanf:"auth_enabled"`
}

// SocketIO holds Socket.IO server settings.
type SocketIO struct {
	PingInterval      time.Duration `koanf:"ping_interval"`
	PingTimeout       time.Duration `koanf:"ping_timeout"`
	MaxHTTPBufferSize int64         `koanf:"max_http_buffer_size"`
	ConnectTimeout    time.Duration `koanf:"connect_timeout"`
	RedisURL          string        `koanf:"redis_url"`
}

// Database holds DB connection settings. Driver selects the GORM dialect.
type Database struct {
	Driver          string        `koanf:"driver"` // postgres, mysql, sqlite, sqlserver, oracle
	DSN             string        `koanf:"dsn"`
	MaxOpenConns    int           `koanf:"max_open_conns"`
	MaxIdleConns    int           `koanf:"max_idle_conns"`
	ConnMaxLifetime time.Duration `koanf:"conn_max_lifetime"`
	LogLevel        string        `koanf:"log_level"` // silent, error, warn, info
}

// Modules holds module discovery and gating settings.
type Modules struct {
	Paths    []string `koanf:"paths"`
	Enabled  []string `koanf:"enabled"`
	Disabled []string `koanf:"disabled"`
}

// Runtime holds agent runtime limits.
type Runtime struct {
	MaxTurns                 int           `koanf:"max_turns"`
	MaxConsecutiveFailures   int           `koanf:"max_consecutive_failures"`
	ToolTimeout              time.Duration `koanf:"tool_timeout"`
	ContextPressureThreshold float64       `koanf:"context_pressure_threshold"`

	// TurnIdleTimeout is the safety watchdog window for a single turn. It is an
	// IDLE bound, not a wall-clock one : it resets every time the turn makes
	// progress (an LLM round or tool batch completes), so a long-but-productive
	// turn runs as long as it needs and only a genuinely STALLED turn (no
	// progress for this whole window) is killed. Must exceed ToolTimeout so a
	// single slow tool is never mistaken for a stall. Default 5m.
	TurnIdleTimeout time.Duration `koanf:"turn_idle_timeout"`

	// TurnPool 3-tier caps (RT-1). Defaults sized for a 32-core
	// machine handling ~1M idle sessions with ~50K active turns at
	// peak. All caps configurable via env (DIGITORN_RUNTIME__...).
	// Zero on any field = unbounded for that tier (acceptable in
	// tests, NEVER in production).
	MaxConcurrentTurnsGlobal  int `koanf:"max_concurrent_turns_global"`   // default 4096
	MaxConcurrentTurnsPerApp  int `koanf:"max_concurrent_turns_per_app"`  // default 256
	MaxConcurrentTurnsPerUser int `koanf:"max_concurrent_turns_per_user"` // default 32

	// Multi-agent delegation caps. MaxConcurrentLLMCalls bounds concurrent
	// LLM calls daemon-wide — the real throttle for a swarm of sub-agents
	// (0 = unbounded). MaxAgentDepth caps delegation nesting ;
	// MaxAgentsPerSession caps the agent tree per root session (anti
	// fork-bomb).
	MaxConcurrentLLMCalls int `koanf:"max_concurrent_llm_calls"` // default 0 (unbounded)
	MaxAgentDepth         int `koanf:"max_agent_depth"`          // default 8
	MaxAgentsPerSession   int `koanf:"max_agents_per_session"`   // default 100000

	// Streaming (R-4) enables per-token EventAssistantDelta emission
	// during a turn when the LLM client supports streaming. The
	// Socket.IO bridge forwards those deltas to connected clients so
	// the UI renders tokens live. Falls back automatically to the
	// synchronous path when the provider/worker can't stream — purely
	// additive, safe to leave on. Default true.
	Streaming bool `koanf:"streaming"`
}

// Logging holds logger settings.
type Logging struct {
	Level  string `koanf:"level"`  // debug, info, warn, error
	Format string `koanf:"format"` // tint, json
}

// LLM groups per-provider credentials.
type LLM struct {
	Providers map[string]map[string]any `koanf:"providers"`
}

// Observability holds metrics / tracing settings.
type Observability struct {
	MetricsEnabled bool   `koanf:"metrics_enabled"`
	MetricsPath    string `koanf:"metrics_path"`
	TracingEnabled bool   `koanf:"tracing_enabled"`
	OTLPEndpoint   string `koanf:"otlp_endpoint"`
}

// defaultDataDir is the per-user, cross-platform home for all daemon runtime
// state (db, sessions, installed apps) : {USER_HOME}/.digitorn. This keeps state
// OUT of the repo / working directory and makes a fresh clone run anywhere without
// machine-specific config. Falls back to ".digitorn" (CWD-relative) only when the
// user home can't be resolved (container without a user dir, etc.).
func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".digitorn")
	}
	return ".digitorn"
}

func defaultAppsRoot() string     { return filepath.Join(defaultDataDir(), "apps") }
func defaultSessionsRoot() string { return filepath.Join(defaultDataDir(), "sessions") }
func defaultDSN() string          { return filepath.Join(defaultDataDir(), "digitorn.db") }

// Defaults returns a configuration with sensible defaults applied.
func Defaults() Config {
	return Config{
		Server: Server{
			Host:            "127.0.0.1",
			Port:            8000,
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    30 * time.Second,
			ShutdownTimeout: 30 * time.Second,
			CORSOrigins:     []string{"http://localhost:3000", "http://localhost:5173"},
			AuthEnabled:     true,
		},
		SocketIO: SocketIO{
			PingInterval:      25 * time.Second,
			PingTimeout:       20 * time.Second,
			MaxHTTPBufferSize: 1_000_000,
			ConnectTimeout:    45 * time.Second,
		},
		Database: Database{
			Driver:          "sqlite",
			DSN:             defaultDSN(),
			MaxOpenConns:    25,
			MaxIdleConns:    5,
			ConnMaxLifetime: 5 * time.Minute,
			LogLevel:        "warn",
		},
		Auth: Auth{
			Enabled:             false,
			Issuer:              "",
			Audience:            "",
			JWKSURL:             "",
			UserIDClaim:         "sub",
			JWKSRefreshInterval: 24 * time.Hour,
			JWKSFailureBackoff:  30 * time.Second,
			JWTLeeway:           60 * time.Second,
			DevMode:             true,
		},
		Apps: Apps{
			Root: defaultAppsRoot(),
			Hub: AppsHub{
				URL:             "https://hub.digitorn.ai",
				Timeout:         60 * time.Second,
				VerifySSL:       true,
				MaxArchiveBytes: 100 * 1024 * 1024, // 100 MB
			},
		},
		Sessions: Sessions{
			Root:                   defaultSessionsRoot(),
			NumShards:              32,
			QueueCapPerShard:       16384,
			BatchMax:               500,
			FlushInterval:          25 * time.Millisecond,
			FDCachePerShard:        512,
			PerSidQuotaPct:         12,
			Fsync:                  true,
			MaxStatesInMemory:      100_000,
			StateIdleEvictAfter:    30 * time.Minute,
			EvictionInterval:       1 * time.Minute,
			SubscriberQueueSize:    1024,
			SubscriberMaxSlowDrops: 100,
		},
		Workers: Workers{
			LLM: WorkerLLM{
				Count:         1,
				GatewayURL:    "",
				Concurrency:   256,
				BufferSize:    16384,
				StartTimeout:  15 * time.Second,
				StopTimeout:   10 * time.Second,
				BackoffMin:    500 * time.Millisecond,
				BackoffMax:    30 * time.Second,
				MaxFailures:   5,
				HealthEvery:   5 * time.Second,
				ClientRetries: 1,
				ClientTimeout: 60 * time.Second,
			},
		},
		Modules: Modules{
			Paths: []string{"internal/modules"},
		},
		Runtime: Runtime{
			MaxTurns:                  200,
			MaxConsecutiveFailures:    8,
			ToolTimeout:               4 * time.Minute,
			TurnIdleTimeout:           5 * time.Minute,
			ContextPressureThreshold:  0.75,
			MaxConcurrentTurnsGlobal:  4096,
			MaxConcurrentTurnsPerApp:  256,
			MaxConcurrentTurnsPerUser: 32,
			Streaming:                 true,
			MaxAgentDepth:             8,
			MaxAgentsPerSession:       100000,
		},
		Logging: Logging{
			Level:  "info",
			Format: "tint",
		},
		Observability: Observability{
			MetricsEnabled: true,
			MetricsPath:    "/metrics",
		},
	}
}

// Load reads configuration from the given YAML path (optional) and overlays
// environment variables. Missing files are not fatal; defaults are always
// applied first.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	cfg := Defaults()
	if err := k.Load(structProvider(&cfg), nil); err != nil {
		return nil, fmt.Errorf("config: load defaults: %w", err)
	}

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
				return nil, fmt.Errorf("config: read %s: %w", path, err)
			}
		}
	}

	// Env vars: DIGITORN_SECTION__KEY -> section.key
	envProvider := env.Provider("DIGITORN_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "DIGITORN_")), "__", ".")
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}

	var out Config
	if err := k.UnmarshalWithConf("", &out, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

// Validate rejects configurations that are internally inconsistent or unsafe
// to run. It runs at the end of Load, after defaults and overrides are merged.
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range (1-65535)", c.Server.Port)
	}
	// Auth on without dev-mode needs a key source — an explicit JWKS URL, or an
	// issuer / service URL to discover one from — else every request fails
	// closed with no way to present a valid token.
	if c.Auth.Enabled && !c.Auth.DevMode &&
		c.Auth.JWKSURL == "" && c.Auth.Issuer == "" && c.Auth.ServiceURL == "" {
		return fmt.Errorf("config: auth.enabled requires one of auth.jwks_url, auth.issuer, auth.service_url (or auth.dev_mode)")
	}
	// A wildcard CORS origin alongside credentials is a real risk, not just a
	// browser no-op for callers that don't honour the spec.
	for _, o := range c.Server.CORSOrigins {
		if o == "*" && len(c.Server.CORSOrigins) > 1 {
			return fmt.Errorf("config: server.cors_origins cannot mix \"*\" with explicit origins")
		}
	}
	if c.Sessions.NumShards < 1 {
		return fmt.Errorf("config: sessions.num_shards must be >= 1 (got %d)", c.Sessions.NumShards)
	}
	if c.Database.Driver == "" {
		return fmt.Errorf("config: database.driver is required")
	}
	return nil
}
