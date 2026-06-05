package db

import (
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// closeOnCleanup closes the underlying *sql.DB so Windows releases the file
// lock before t.TempDir's RemoveAll runs (cleanups fire LIFO).
func closeOnCleanup(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

// legacyCredentialsDDL mirrors the old Python daemon's credentials table —
// a shape GORM's SQLite migrator cannot rebuild into the Go schema. Boot must
// not die on it.
const legacyCredentialsDDL = `CREATE TABLE credentials (
	id VARCHAR(64) NOT NULL,
	user_id VARCHAR(64),
	app_id VARCHAR(255),
	provider_name VARCHAR(128) NOT NULL,
	provider_type VARCHAR(32) NOT NULL,
	scope VARCHAR(32) NOT NULL,
	name VARCHAR(64) DEFAULT '' NOT NULL,
	owner_type VARCHAR(16) NOT NULL,
	label VARCHAR(64) NOT NULL,
	encrypted_fields BLOB NOT NULL,
	nonce BLOB NOT NULL,
	status VARCHAR(32) NOT NULL,
	display_metadata JSON NOT NULL,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL,
	PRIMARY KEY (id)
)`

// TestAutoMigrate_LegacyPythonCredentialsTable : a DB carrying the old Python
// credentials table must migrate cleanly — the legacy table is renamed aside
// (its row preserved), a fresh Go-schema credentials table is created, and a
// second AutoMigrate is a no-op (idempotent). This is the boot path that used
// to fail with "NOT NULL constraint failed: credentials__temp.provider_name".
func TestAutoMigrate_LegacyPythonCredentialsTable(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	gdb, err := Open(Options{Driver: DriverSQLite, DSN: dsn, LogLevel: "silent"}, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeOnCleanup(t, gdb)

	// Seed the legacy Python table + one real row (an encrypted deepseek key).
	if err := gdb.Exec(legacyCredentialsDDL).Error; err != nil {
		t.Fatalf("seed legacy ddl: %v", err)
	}
	if err := gdb.Exec(`INSERT INTO credentials
		(id,user_id,provider_name,provider_type,scope,name,owner_type,label,encrypted_fields,nonce,status,display_metadata,created_at,updated_at)
		VALUES ('id1','u1','deepseek','api_key','system_wide','','user','ds',x'00',x'00','active','{}','2026-01-01','2026-01-01')`).Error; err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := AutoMigrate(gdb); err != nil {
		t.Fatalf("AutoMigrate over legacy DB must succeed, got: %v", err)
	}

	// The Go-schema table now exists and is usable (insert via the model).
	if !gdb.Migrator().HasColumn(&models.Credential{}, "provider") ||
		!gdb.Migrator().HasColumn(&models.Credential{}, "fields") {
		t.Fatalf("fresh Go credentials table missing provider/fields columns")
	}

	// The legacy row was preserved, not lost.
	var preserved int64
	if err := gdb.Raw("SELECT COUNT(*) FROM credentials_legacy_py WHERE provider_name='deepseek'").Scan(&preserved).Error; err != nil {
		t.Fatalf("query preserved legacy: %v", err)
	}
	if preserved != 1 {
		t.Fatalf("legacy deepseek row must be preserved in credentials_legacy_py, got %d", preserved)
	}

	// The fresh Go table starts empty (gateway/remote mode needs no DB cred).
	var fresh int64
	gdb.Model(&models.Credential{}).Count(&fresh)
	if fresh != 0 {
		t.Fatalf("fresh Go credentials table should be empty, got %d", fresh)
	}

	// Idempotent : a second migrate is a clean no-op (no stray-table churn).
	if err := AutoMigrate(gdb); err != nil {
		t.Fatalf("second AutoMigrate must be a no-op, got: %v", err)
	}
}

// TestAutoMigrate_FreshDB : with no pre-existing tables, AutoMigrate just
// builds the Go schema — no legacy reconcile side effects.
func TestAutoMigrate_FreshDB(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fresh.db")
	gdb, err := Open(Options{Driver: DriverSQLite, DSN: dsn, LogLevel: "silent"}, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeOnCleanup(t, gdb)
	if err := AutoMigrate(gdb); err != nil {
		t.Fatalf("fresh AutoMigrate: %v", err)
	}
	if gdb.Migrator().HasTable("credentials_legacy_py") {
		t.Fatalf("fresh DB must not create a legacy table")
	}
	if !gdb.Migrator().HasColumn(&models.Credential{}, "fields") {
		t.Fatalf("fresh DB missing Go credentials schema")
	}
}
