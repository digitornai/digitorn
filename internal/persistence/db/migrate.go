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
	if err := dropDriftedUserTable(gdb, &models.UserSkill{}, "user_skills", "idx_uskill_user_app_name"); err != nil {
		return fmt.Errorf("db: reconcile user_skills: %w", err)
	}
	if err := dropDriftedUserTable(gdb, &models.UserSnippet{}, "user_snippets", "idx_usnip_user_app"); err != nil {
		return fmt.Errorf("db: reconcile user_snippets: %w", err)
	}
	if err := gdb.AutoMigrate(
		&models.App{},
		&models.Credential{},
		&models.OAuthState{},
		&models.AuditLog{},
		&models.ModuleState{},
		&models.UserSkill{},
		&models.UserSnippet{},
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

// dropDriftedUserTable drops a per-user table (user_skills / user_snippets) whose
// on-disk schema drifted from the Go model — specifically a NULLABLE user_id or a
// missing index. GORM's SQLite migrator tries to rebuild such a table via a __temp
// copy but omits user_id from the copy, failing boot with a NOT NULL constraint
// error. These tables hold per-user-app convenience data the app regenerates, so
// dropping is safe ; AutoMigrate then recreates the table with the correct schema.
// A table that already matches (user_id NOT NULL + the named index) is left as-is.
func dropDriftedUserTable(gdb *gorm.DB, model any, table, idxName string) error {
	m := gdb.Migrator()
	if !m.HasTable(model) {
		return nil // fresh install — AutoMigrate creates it
	}
	cols, err := m.ColumnTypes(model)
	if err != nil {
		return err
	}
	userIDNotNull := false
	for _, c := range cols {
		if strings.EqualFold(c.Name(), "user_id") {
			if n, ok := c.Nullable(); ok && !n {
				userIDNotNull = true
			}
		}
	}
	if userIDNotNull && m.HasIndex(model, idxName) {
		return nil // current schema — leave it
	}
	slog.Warn("db: "+table+" schema drift (GORM can't migrate it) — dropping for a fresh recreate",
		slog.Bool("user_id_not_null", userIDNotNull), slog.Bool("has_index", m.HasIndex(model, idxName)))
	return gdb.Exec("DROP TABLE IF EXISTS " + table).Error
}
