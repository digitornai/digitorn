package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/mbathepaul/digitorn/internal/background/runner"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// Inbound is the optional inbound side (the adapter manager): it serves channel
// HTTP routes and runs the adapters. Kept as an interface so the service stays
// decoupled from the concrete processor/adapter packages.
type Inbound interface {
	Start(ctx context.Context) error
	Handler() http.Handler // may return nil when no HTTP adapter is registered
}

// Setup is what the caller's build func returns: the job processor (built from
// the freshly-opened store) and, optionally, the inbound side.
type Setup struct {
	Processor runner.Processor
	Inbound   Inbound
}

// Service is the assembled background service: durable store + worker pool + an
// HTTP control surface (+ optional inbound adapters), with graceful shutdown.
type Service struct {
	cfg     Config
	log     *slog.Logger
	store   *store.Store
	pool    *runner.Pool
	httpd   *http.Server
	closeDB func() error
	started time.Time
	inbound Inbound
}

// New opens the DB (SQLite or Postgres, by config), migrates, lets the caller
// build the processor + inbound from the store, and assembles the pool. The
// build func receives the store so a store-dependent processor (the channel
// processor, which loads trigger configs) can be constructed.
func New(cfg Config, build func(*store.Store) (Setup, error), log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	gdb, closeDB, err := openDB(cfg)
	if err != nil {
		return nil, err
	}
	st := store.New(gdb)
	if err := st.Migrate(); err != nil {
		_ = closeDB()
		return nil, fmt.Errorf("background: migrate: %w", err)
	}
	setup, err := build(st)
	if err != nil {
		_ = closeDB()
		return nil, fmt.Errorf("background: build: %w", err)
	}
	if setup.Processor == nil {
		_ = closeDB()
		return nil, fmt.Errorf("background: build returned a nil processor")
	}
	pool := runner.New(st, setup.Processor, runner.Options{
		Workers:  cfg.Workers,
		LeaseTTL: cfg.LeaseTTL,
		Logger:   log,
	})
	s := &Service{
		cfg:     cfg,
		log:     log,
		store:   st,
		pool:    pool,
		closeDB: closeDB,
		started: time.Now(),
		inbound: setup.Inbound,
	}
	s.httpd = &http.Server{Addr: cfg.HTTPAddr, Handler: s.mux(), ReadHeaderTimeout: 5 * time.Second}
	return s, nil
}

// openDB opens GORM directly (no daemon coupling): SQLite for a local daemon
// (single writer → MaxOpenConns(1)), Postgres for cloud. Returns a close func.
func openDB(cfg Config) (*gorm.DB, func() error, error) {
	var dial gorm.Dialector
	switch cfg.DBDriver {
	case "sqlite", "":
		dial = sqlite.Open(cfg.DBDSN)
	case "postgres":
		dial = postgres.Open(cfg.DBDSN)
	default:
		return nil, nil, fmt.Errorf("background: unknown DB driver %q (sqlite|postgres)", cfg.DBDriver)
	}
	gdb, err := gorm.Open(dial, &gorm.Config{
		Logger:  gormlogger.Default.LogMode(gormlogger.Warn),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, nil, fmt.Errorf("background: open db: %w", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, nil, err
	}
	if cfg.DBDriver == "sqlite" || cfg.DBDriver == "" {
		sqlDB.SetMaxOpenConns(1) // SQLite is single-writer
	}
	return gdb, sqlDB.Close, nil
}

func (s *Service) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"status": "ok", "uptime_sec": int(time.Since(s.started).Seconds())})
	})
	m.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		counts, _ := s.store.Counts(r.Context())
		writeJSON(w, map[string]any{
			"pool":       s.pool.Stats(),
			"jobs":       counts,
			"uptime_sec": int(time.Since(s.started).Seconds()),
		})
	})
	// Inbound adapter HTTP routes (webhook). Mounted as the catch-all; the
	// specific /healthz and /stats above take precedence (ServeMux longest-match).
	if s.inbound != nil {
		if h := s.inbound.Handler(); h != nil {
			m.Handle("/", h)
		}
	}
	return m
}

// Run starts the HTTP surface and drains jobs until ctx is cancelled, then
// shuts the HTTP server down and closes the DB. Blocking.
func (s *Service) Run(ctx context.Context) error {
	go func() {
		if err := s.httpd.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("background: http", "err", err.Error())
		}
	}()
	s.log.Info("background service up",
		"http", s.cfg.HTTPAddr, "workers", s.cfg.Workers, "driver", s.cfg.DBDriver)

	if s.inbound != nil {
		go func() {
			if err := s.inbound.Start(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("background: inbound", "err", err.Error())
			}
		}()
	}

	s.pool.Run(ctx) // returns when ctx is cancelled and in-flight work has drained

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpd.Shutdown(sctx)
	s.log.Info("background service stopped")
	return s.closeDB()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
