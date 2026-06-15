// Package service wires the background components — durable store + worker pool
// + an HTTP control surface — into one standalone, gracefully-shutdownable
// service. It imports NOTHING from the daemon (internal/server, internal/runtime):
// the daemon is reached only later, over its public HTTP API (BG-3). The only
// shared deps are generic libraries (GORM, the drivers).
package service

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config is the background service's configuration. Every field has an env
// override (DIGITORN_BG_*) with a local-first default, so a single-user local
// daemon runs with zero configuration (SQLite next to the binary).
type Config struct {
	DBDriver string // "sqlite" (local) | "postgres" (cloud)
	DBDSN    string // sqlite file path or postgres DSN
	HTTPAddr string // control surface (/healthz, /stats)
	Workers  int
	LeaseTTL time.Duration

	// For BG-3 (the daemon client): how to reach the daemon + the service identity.
	// ServiceJWT must be a dedicated SERVICE token (role "service" or permission
	// "sessions:impersonate") so the daemon authorises waking a session AS its
	// end-user (X-Act-As-User) — never a human/admin token. A mis-scoped token is
	// flagged at boot (CanImpersonate) and 403s every user-owned wake.
	DaemonURL  string
	ServiceJWT string

	// For BG-6 (config discovery): where the app bundles live + how often to
	// re-scan them to pick up installs / channel-config changes.
	AppsDir   string
	RescanSec int

	// OpsToken, when set, is required as a Bearer credential on every /ops route
	// (the management + observability API). Empty → /ops is open, matching the
	// existing localhost /stats surface; set it for a network-reachable deployment.
	OpsToken string
}

// FromEnv builds a Config from DIGITORN_BG_* env vars with sane defaults.
func FromEnv() Config {
	return Config{
		DBDriver:   env("DIGITORN_BG_DB_DRIVER", "sqlite"),
		DBDSN:      env("DIGITORN_BG_DB_DSN", "digitorn-background.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"),
		HTTPAddr:   env("DIGITORN_BG_HTTP_ADDR", "127.0.0.1:8090"),
		Workers:    envInt("DIGITORN_BG_WORKERS", 16),
		LeaseTTL:   time.Duration(envInt("DIGITORN_BG_LEASE_TTL_SEC", 60)) * time.Second,
		DaemonURL:  env("DIGITORN_BG_DAEMON_URL", "http://127.0.0.1:8000"),
		ServiceJWT: os.Getenv("DIGITORN_BG_SERVICE_JWT"),
		AppsDir:    env("DIGITORN_BG_APPS_DIR", defaultAppsDir()),
		RescanSec:  envInt("DIGITORN_BG_RESCAN_SEC", 60),
		OpsToken:   os.Getenv("DIGITORN_BG_OPS_TOKEN"),
	}
}

// defaultAppsDir points at the daemon's app bundles (~/.digitorn/apps), which
// the background service reads to discover channel configs.
func defaultAppsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "apps"
	}
	return filepath.Join(home, ".digitorn", "apps")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
