package db

import (
	"path/filepath"
	"testing"
)

func TestOpen_SQLite_PingsSuccessfully(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	gdb, err := Open(Options{Driver: DriverSQLite, DSN: dsn, LogLevel: "silent"}, nil)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping after open: %v", err)
	}
	_ = sqlDB.Close()
}

func TestOpen_UnknownDriver_Errors(t *testing.T) {
	if _, err := Open(Options{Driver: "nope", DSN: "x"}, nil); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

// TestOpen_UnreachablePostgres_FailsEagerly proves the Ping surfaces a bad
// connection at Open time instead of deferring to the first query. Postgres at
// a closed port must fail fast.
func TestOpen_UnreachablePostgres_FailsEagerly(t *testing.T) {
	// 127.0.0.1:1 is reserved/closed — the dial fails immediately.
	dsn := "host=127.0.0.1 port=1 user=x password=y dbname=z sslmode=disable connect_timeout=2"
	if _, err := Open(Options{Driver: DriverPostgres, DSN: dsn, LogLevel: "silent"}, nil); err == nil {
		t.Fatal("expected eager ping failure for an unreachable postgres, got nil")
	}
}
