package db

import (
	"fmt"
	"log/slog"
	"strings"

	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// AutoMigrate runs GORM's AutoMigrate over metadata-only tables. Session
// and message runtime data live on disk (sessionstore), NOT here.
// Suitable for development; production uses goose SQL migrations.
func AutoMigrate(gdb *gorm.DB) error {
	if err := reconcileLegacyCredentials(gdb); err != nil {
		return fmt.Errorf("db: reconcile legacy credentials: %w", err)
	}
	if err := gdb.AutoMigrate(
		&models.App{},
		&models.Credential{},
		&models.AuditLog{},
		&models.ModuleState{},
	); err != nil {
		return fmt.Errorf("db: auto-migrate: %w", err)
	}
	return nil
}

// reconcileLegacyCredentials moves a legacy Python `credentials` table out of
// the way so GORM can build the Go schema cleanly. The old Python daemon
// shipped a wholly different credentials table (provider_name / provider_type /
// nonce / encrypted_fields / display_metadata …) ; GORM's SQLite migrator
// chokes trying to rebuild it into the Go shape (provider / fields), failing
// boot with a NOT NULL constraint error on the temp table. We detect that
// legacy shape and rename it aside — data is preserved (never dropped on first
// sight), the rename is idempotent across restarts, and a fresh empty Go-schema
// table is created by AutoMigrate. Auth in gateway/remote mode uses the JWT in
// credentials.json, not this table, so an empty table is correct for that path.
func reconcileLegacyCredentials(gdb *gorm.DB) error {
	m := gdb.Migrator()
	if !m.HasTable(&models.Credential{}) {
		return nil // fresh install — AutoMigrate creates the Go table
	}
	cols, err := m.ColumnTypes(&models.Credential{})
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(cols))
	for _, c := range cols {
		have[strings.ToLower(c.Name())] = true
	}
	// Legacy marker : the Python-only column is present AND the Go-only column
	// is absent. A table that already has `fields` is the Go schema — leave it.
	if !have["provider_name"] || have["fields"] {
		return nil
	}

	const legacy = "credentials_legacy_py"
	if m.HasTable(legacy) {
		// The legacy rows are already preserved from an earlier run ; this
		// stray legacy `credentials` only blocks the Go schema — drop it.
		slog.Warn("db: dropping stray legacy credentials table (already preserved)",
			slog.String("preserved_as", legacy))
		return gdb.Exec("DROP TABLE credentials").Error
	}
	slog.Warn("db: legacy Python credentials table detected — renaming aside so the Go schema can be created (data preserved)",
		slog.String("renamed_to", legacy))
	return gdb.Exec("ALTER TABLE credentials RENAME TO " + legacy).Error
}
