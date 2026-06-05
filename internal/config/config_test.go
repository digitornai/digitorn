package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/config"
)

// H4 — Hardening config : YAML parsing, env override, defaults,
// multi-env, type coercion. The config package is the daemon's
// boot contract — every wrong value here means the daemon either
// fails to start or starts in a wrong configuration. These tests
// pin every documented behavior.

// TestConfig_DefaultsAreApplied verifies that calling Load with an
// empty path (and no env vars set) yields exactly the documented
// defaults. This is the boot path on first install.
func TestConfig_DefaultsAreApplied(t *testing.T) {
	clearDigitornEnv(t)

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Server.Host = %q, want 127.0.0.1", cfg.Server.Host)
	}
	if cfg.Server.Port != 8000 {
		t.Errorf("Server.Port = %d, want 8000", cfg.Server.Port)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("Database.Driver = %q, want sqlite", cfg.Database.Driver)
	}
	if cfg.Sessions.NumShards != 32 {
		t.Errorf("Sessions.NumShards = %d, want 32", cfg.Sessions.NumShards)
	}
	if !cfg.Sessions.Fsync {
		t.Error("Sessions.Fsync default must be true (event sourcing durability)")
	}
	if cfg.Sessions.FlushInterval != 25*time.Millisecond {
		t.Errorf("Sessions.FlushInterval = %v, want 25ms", cfg.Sessions.FlushInterval)
	}
	if cfg.Workers.LLM.Count != 1 {
		t.Errorf("Workers.LLM.Count = %d, want 1", cfg.Workers.LLM.Count)
	}
	if cfg.Auth.UserIDClaim != "sub" {
		t.Errorf("Auth.UserIDClaim = %q, want sub", cfg.Auth.UserIDClaim)
	}
	if len(cfg.Server.CORSOrigins) == 0 {
		t.Error("Server.CORSOrigins default must include localhost dev origins")
	}
}

// TestConfig_YAMLFileOverridesDefaults verifies that values in a YAML
// file replace defaults for each declared field, leaving undeclared
// fields at their default values.
func TestConfig_YAMLFileOverridesDefaults(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, `
server:
  host: 0.0.0.0
  port: 9090
  read_timeout: 45s
sessions:
  num_shards: 64
  fsync: false
auth:
  enabled: true
  issuer: https://auth.digitorn.ai
  dev_mode: false
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("Server.Host = %q, want 0.0.0.0", cfg.Server.Host)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("Server.Port = %d, want 9090", cfg.Server.Port)
	}
	if cfg.Server.ReadTimeout != 45*time.Second {
		t.Errorf("Server.ReadTimeout = %v, want 45s", cfg.Server.ReadTimeout)
	}
	if cfg.Sessions.NumShards != 64 {
		t.Errorf("Sessions.NumShards = %d, want 64", cfg.Sessions.NumShards)
	}
	if cfg.Sessions.Fsync {
		t.Error("Sessions.Fsync should be false after YAML override")
	}
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled should be true after YAML override")
	}
	if cfg.Auth.Issuer != "https://auth.digitorn.ai" {
		t.Errorf("Auth.Issuer = %q", cfg.Auth.Issuer)
	}
	if cfg.Auth.DevMode {
		t.Error("Auth.DevMode should be false after YAML override")
	}
	// Undeclared fields keep defaults.
	if cfg.Server.WriteTimeout != 30*time.Second {
		t.Errorf("WriteTimeout should keep default, got %v", cfg.Server.WriteTimeout)
	}
	if cfg.Workers.LLM.Count != 1 {
		t.Errorf("Workers.LLM.Count should keep default, got %d", cfg.Workers.LLM.Count)
	}
}

// TestConfig_EnvVarOverridesYAML verifies the priority chain :
//
//	defaults < YAML file < env vars
//
// Env vars are the last layer applied so deployment overrides win.
func TestConfig_EnvVarOverridesYAML(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, `
server:
  port: 9090
sessions:
  num_shards: 64
`)
	t.Setenv("DIGITORN_SERVER__PORT", "12345")
	t.Setenv("DIGITORN_SESSIONS__NUM_SHARDS", "128")
	t.Setenv("DIGITORN_AUTH__ENABLED", "true")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 12345 {
		t.Errorf("env override failed : Server.Port = %d, want 12345", cfg.Server.Port)
	}
	if cfg.Sessions.NumShards != 128 {
		t.Errorf("env override failed : NumShards = %d, want 128", cfg.Sessions.NumShards)
	}
	if !cfg.Auth.Enabled {
		t.Error("env override failed : Auth.Enabled should be true")
	}
}

// TestConfig_EnvVarFormat verifies the DIGITORN_<SECTION>__<KEY>
// convention with double underscores as the nested-key delimiter.
func TestConfig_EnvVarFormat(t *testing.T) {
	clearDigitornEnv(t)
	t.Setenv("DIGITORN_DATABASE__DSN", "postgres://prod-db:5432/digitorn")
	t.Setenv("DIGITORN_OBSERVABILITY__METRICS_PATH", "/_/metrics")
	t.Setenv("DIGITORN_WORKERS__LLM__COUNT", "8")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != "postgres://prod-db:5432/digitorn" {
		t.Errorf("DSN = %q", cfg.Database.DSN)
	}
	if cfg.Observability.MetricsPath != "/_/metrics" {
		t.Errorf("MetricsPath = %q", cfg.Observability.MetricsPath)
	}
	if cfg.Workers.LLM.Count != 8 {
		t.Errorf("Workers.LLM.Count = %d, want 8", cfg.Workers.LLM.Count)
	}
}

// TestConfig_MissingFileIsNotFatal verifies that calling Load with a
// path that doesn't exist still returns defaults rather than an error.
// This is the convenience of "ship without a config file".
func TestConfig_MissingFileIsNotFatal(t *testing.T) {
	clearDigitornEnv(t)
	cfg, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing file returned error: %v", err)
	}
	if cfg.Server.Port != 8000 {
		t.Errorf("defaults not applied : Port = %d", cfg.Server.Port)
	}
}

// TestConfig_InvalidYAMLErrors verifies that a syntactically broken
// YAML produces a clear error rather than a misleading default.
func TestConfig_InvalidYAMLErrors(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, "server:\n  port: \"unterminated string\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error on malformed YAML")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error should mention 'config' context, got: %v", err)
	}
}

// TestConfig_DurationTypeCoercion verifies that duration strings in
// YAML and env vars are correctly parsed into time.Duration.
func TestConfig_DurationTypeCoercion(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, `
server:
  read_timeout: 90s
  shutdown_timeout: 5m
sessions:
  flush_interval: 100ms
  state_idle_evict_after: 1h
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ReadTimeout != 90*time.Second {
		t.Errorf("ReadTimeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.ShutdownTimeout != 5*time.Minute {
		t.Errorf("ShutdownTimeout = %v", cfg.Server.ShutdownTimeout)
	}
	if cfg.Sessions.FlushInterval != 100*time.Millisecond {
		t.Errorf("FlushInterval = %v", cfg.Sessions.FlushInterval)
	}
	if cfg.Sessions.StateIdleEvictAfter != time.Hour {
		t.Errorf("StateIdleEvictAfter = %v", cfg.Sessions.StateIdleEvictAfter)
	}
}

// TestConfig_DurationFromEnvVar verifies env var → duration coercion.
func TestConfig_DurationFromEnvVar(t *testing.T) {
	clearDigitornEnv(t)
	t.Setenv("DIGITORN_SERVER__READ_TIMEOUT", "120s")
	t.Setenv("DIGITORN_SESSIONS__FLUSH_INTERVAL", "5ms")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ReadTimeout != 120*time.Second {
		t.Errorf("ReadTimeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Sessions.FlushInterval != 5*time.Millisecond {
		t.Errorf("FlushInterval = %v", cfg.Sessions.FlushInterval)
	}
}

// TestConfig_SliceFields verifies that YAML lists decode into slices.
func TestConfig_SliceFields(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, `
server:
  cors_origins:
    - https://app.digitorn.ai
    - https://staging.digitorn.ai
modules:
  paths:
    - /opt/digitorn/modules
    - /usr/local/share/digitorn/modules
  enabled:
    - llm_provider
    - filesystem
    - shell
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Server.CORSOrigins) != 2 {
		t.Fatalf("CORSOrigins len = %d", len(cfg.Server.CORSOrigins))
	}
	if cfg.Server.CORSOrigins[0] != "https://app.digitorn.ai" {
		t.Errorf("CORSOrigins[0] = %q", cfg.Server.CORSOrigins[0])
	}
	if len(cfg.Modules.Paths) != 2 || len(cfg.Modules.Enabled) != 3 {
		t.Errorf("Modules slices : paths=%d enabled=%d", len(cfg.Modules.Paths), len(cfg.Modules.Enabled))
	}
}

// TestConfig_MapFields verifies that the LLM.Providers map (nested
// dynamic config per provider) decodes correctly.
func TestConfig_MapFields(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, `
llm:
  providers:
    anthropic:
      api_key: env:ANTHROPIC_API_KEY
      base_url: https://api.anthropic.com
    openai:
      api_key: env:OPENAI_API_KEY
      organization: org-1234
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.LLM.Providers) != 2 {
		t.Fatalf("Providers len = %d", len(cfg.LLM.Providers))
	}
	ant, ok := cfg.LLM.Providers["anthropic"]
	if !ok {
		t.Fatal("Providers missing 'anthropic' key")
	}
	if ant["api_key"] != "env:ANTHROPIC_API_KEY" {
		t.Errorf("anthropic.api_key = %v", ant["api_key"])
	}
}

// TestConfig_BoolCoercion verifies bool fields parse from YAML
// (true/false) and env vars ("true"/"false"/"1"/"0").
func TestConfig_BoolCoercion(t *testing.T) {
	clearDigitornEnv(t)

	t.Run("yaml_true_false", func(t *testing.T) {
		path := writeTempYAML(t, `
sessions:
  fsync: false
auth:
  enabled: true
  dev_mode: false
  jwks_url: https://auth.example.test/.well-known/jwks.json
observability:
  metrics_enabled: false
  tracing_enabled: true
`)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Sessions.Fsync {
			t.Error("Sessions.Fsync")
		}
		if !cfg.Auth.Enabled {
			t.Error("Auth.Enabled")
		}
		if cfg.Auth.DevMode {
			t.Error("Auth.DevMode")
		}
		if cfg.Observability.MetricsEnabled {
			t.Error("MetricsEnabled")
		}
		if !cfg.Observability.TracingEnabled {
			t.Error("TracingEnabled")
		}
	})

	t.Run("env_true_false", func(t *testing.T) {
		t.Setenv("DIGITORN_SESSIONS__FSYNC", "false")
		t.Setenv("DIGITORN_AUTH__ENABLED", "true")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Sessions.Fsync {
			t.Error("Sessions.Fsync via env")
		}
		if !cfg.Auth.Enabled {
			t.Error("Auth.Enabled via env")
		}
	})
}

// TestConfig_NumericRanges verifies that numeric fields accept their
// documented ranges without truncation or overflow.
func TestConfig_NumericRanges(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, `
sessions:
  queue_cap_per_shard: 1048576
  max_states_in_memory: 1000000
  subscriber_max_slow_drops: 18446744073709551614
runtime:
  context_pressure_threshold: 0.95
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sessions.QueueCapPerShard != 1_048_576 {
		t.Errorf("QueueCapPerShard = %d", cfg.Sessions.QueueCapPerShard)
	}
	if cfg.Sessions.MaxStatesInMemory != 1_000_000 {
		t.Errorf("MaxStatesInMemory = %d", cfg.Sessions.MaxStatesInMemory)
	}
	if cfg.Sessions.SubscriberMaxSlowDrops != 18_446_744_073_709_551_614 {
		t.Errorf("SubscriberMaxSlowDrops = %d", cfg.Sessions.SubscriberMaxSlowDrops)
	}
	if cfg.Runtime.ContextPressureThreshold != 0.95 {
		t.Errorf("ContextPressureThreshold = %v", cfg.Runtime.ContextPressureThreshold)
	}
}

// TestConfig_MultiEnvironmentLayering simulates the prod-vs-dev story :
// a base config file + an environment-specific overlay via env vars.
// Verify that the result is the union (env wins on overlap).
func TestConfig_MultiEnvironmentLayering(t *testing.T) {
	clearDigitornEnv(t)
	base := writeTempYAML(t, `
server:
  port: 8000
auth:
  enabled: false
  dev_mode: true
database:
  driver: sqlite
  dsn: digitorn.db
`)
	// "production" overlay : flip relevant flags via env.
	t.Setenv("DIGITORN_AUTH__ENABLED", "true")
	t.Setenv("DIGITORN_AUTH__DEV_MODE", "false")
	t.Setenv("DIGITORN_AUTH__JWKS_URL", "https://auth.example.test/.well-known/jwks.json")
	t.Setenv("DIGITORN_DATABASE__DRIVER", "postgres")
	t.Setenv("DIGITORN_DATABASE__DSN", "postgres://prod/digitorn?sslmode=require")

	cfg, err := config.Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Base file value preserved.
	if cfg.Server.Port != 8000 {
		t.Errorf("Server.Port should keep base = 8000, got %d", cfg.Server.Port)
	}
	// Env-overlaid values win.
	if !cfg.Auth.Enabled || cfg.Auth.DevMode {
		t.Errorf("Auth overlay failed : Enabled=%v DevMode=%v", cfg.Auth.Enabled, cfg.Auth.DevMode)
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q, want postgres", cfg.Database.Driver)
	}
	if !strings.HasPrefix(cfg.Database.DSN, "postgres://") {
		t.Errorf("DSN should be postgres, got %q", cfg.Database.DSN)
	}
}

// TestConfig_EmptyYAMLFileUsesDefaults verifies that an empty (but
// existing) YAML file behaves like no file at all.
func TestConfig_EmptyYAMLFileUsesDefaults(t *testing.T) {
	clearDigitornEnv(t)
	path := writeTempYAML(t, "")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load empty file: %v", err)
	}
	if cfg.Server.Port != 8000 {
		t.Errorf("Server.Port = %d, want 8000", cfg.Server.Port)
	}
	if cfg.Workers.LLM.Count != 1 {
		t.Errorf("Workers.LLM.Count = %d, want 1", cfg.Workers.LLM.Count)
	}
}

// ----- helpers -----

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// clearDigitornEnv removes any DIGITORN_* env vars that may leak from
// the developer's shell or a previous test. We use t.Setenv("", "")
// equivalents : actually unset via direct os calls because Setenv
// only restores at test end. We snapshot first to restore on cleanup.
func clearDigitornEnv(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DIGITORN_") {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		v := kv[eq+1:]
		saved[k] = v
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}
