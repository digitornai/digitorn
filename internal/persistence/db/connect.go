// Package db connects to a database via GORM. Multi-driver: postgres, mysql,
// sqlite, sqlserver are built-in. Oracle requires the oracle-samples/gorm-oracle
// driver (CGO + Oracle Instant Client) and is enabled via the "oracle" build tag.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlserver"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Driver names supported by the daemon.
const (
	DriverPostgres  = "postgres"
	DriverMySQL     = "mysql"
	DriverSQLite    = "sqlite"
	DriverSQLServer = "sqlserver"
	DriverOracle    = "oracle"
)

// Options configures the DB connection.
type Options struct {
	Driver          string
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	LogLevel        string // silent, error, warn, info
}

// Open opens a GORM database connection for the given driver.
// Returns *gorm.DB ready to use, or an error if the driver is unknown or the
// connection fails.
func Open(opts Options, logger *slog.Logger) (*gorm.DB, error) {
	dialector, err := dialect(opts.Driver, opts.DSN)
	if err != nil {
		return nil, err
	}

	gcfg := &gorm.Config{
		Logger: newGormLogger(logger, opts.LogLevel),
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	}

	gdb, err := gorm.Open(dialector, gcfg)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", opts.Driver, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("db: get sql.DB: %w", err)
	}
	if opts.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	}
	if opts.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	}
	if opts.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}

	// Verify connectivity eagerly. gorm.Open is lazy for the network drivers
	// (postgres/mysql/sqlserver) : a wrong DSN or unreachable host otherwise
	// succeeds here and only fails on the first query, far from the cause. A
	// short-bounded Ping surfaces the real error at startup. (SQLite is local,
	// so Ping is a cheap no-op there.)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("db: ping %s: %w", opts.Driver, err)
	}

	return gdb, nil
}

func dialect(driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case DriverPostgres:
		return postgres.Open(dsn), nil
	case DriverMySQL:
		return mysql.Open(dsn), nil
	case DriverSQLite:
		// SQLite returns CANTOPEN (surfaced confusingly as "out of memory (14)")
		// when the DB file's PARENT dir is missing — exactly what a fresh clone hits
		// when its configured data dir doesn't exist yet. Create it first.
		ensureSQLiteDir(dsn)
		return sqlite.Open(dsn), nil
	case DriverSQLServer:
		return sqlserver.Open(dsn), nil
	case DriverOracle:
		return oracleDialect(dsn)
	default:
		return nil, fmt.Errorf("db: unknown driver %q (supported: postgres, mysql, sqlite, sqlserver, oracle)", driver)
	}
}

// ensureSQLiteDir creates the parent directory of a sqlite DB file so opening it
// never fails on a missing dir. No-op for :memory: or an empty/relative-cwd path.
func ensureSQLiteDir(dsn string) {
	p := strings.TrimPrefix(strings.TrimSpace(dsn), "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == "" || strings.HasPrefix(p, ":") {
		return
	}
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
}

func newGormLogger(logger *slog.Logger, level string) gormlogger.Interface {
	var lvl gormlogger.LogLevel
	switch level {
	case "silent":
		lvl = gormlogger.Silent
	case "error":
		lvl = gormlogger.Error
	case "info":
		lvl = gormlogger.Info
	default:
		lvl = gormlogger.Warn
	}
	return gormlogger.Default.LogMode(lvl)
}
