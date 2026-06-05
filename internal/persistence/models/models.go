// Package models contains GORM models for the daemon's persistence layer.
// Sessions and messages are NOT stored here — they live on disk via
// internal/runtime/sessionstore. Postgres holds only deployment metadata,
// credentials, audit logs, and per-app module state.
package models

import (
	"time"

	"github.com/google/uuid"
)

// App is one installed app. Radically simple : app_id is the primary
// key, install overwrites previous version, no history, no scope, no
// owner. The source YAML + assets live on disk under {Apps.Root}/{app_id}/
// — the DB only stores enough metadata to list apps and toggle them.
type App struct {
	AppID       string `gorm:"size:128;primaryKey"`
	Name        string `gorm:"size:256"`
	Version     string `gorm:"size:64"`
	Description string `gorm:"type:text"`

	// Denormalized metadata for the marketplace UI (avoids parsing the
	// bundle on every /api/apps call).
	Category string `gorm:"size:64;index"`
	Author   string `gorm:"size:128"`
	Icon     string `gorm:"size:256"` // file path (icon.png) OR text/emoji (🧱)
	Color    string `gorm:"size:16"`  // hex color, e.g. #14B8A6 — used when Icon is text/emoji

	Enabled bool `gorm:"not null;default:true"`

	// BYOK (Bring Your Own Key) is a per-installation routing decision.
	// false (default) : the daemon routes LLM traffic through the
	//                   digitorn LLM gateway. The operator/user is
	//                   expected to be a signed-in digitorn user whose
	//                   JWT covers credential resolution at the gateway.
	// true            : the daemon dials the provider directly using
	//                   the brain-declared credential from the bundle.
	//                   Used by self-hosted / local-only deployments
	//                   where the user supplies their own API keys.
	// Toggled at runtime via PUT /api/apps/{app_id}/byok ; persisted
	// across daemon restarts.
	BYOK bool `gorm:"not null;default:false"`

	InstalledAt time.Time
	UpdatedAt   time.Time
}

func (App) TableName() string { return "apps" }

// Credential is an encrypted per-user credential for a provider.
type Credential struct {
	ID        uuid.UUID `gorm:"type:char(36);primaryKey"`
	UserID    string    `gorm:"size:128;not null;uniqueIndex:idx_cred_user_provider,priority:1"`
	Provider  string    `gorm:"size:128;not null;uniqueIndex:idx_cred_user_provider,priority:2"`
	Fields    []byte    `gorm:"type:text"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (Credential) TableName() string { return "credentials" }

// AuditLog records all sensitive actions for compliance.
type AuditLog struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement"`
	Timestamp time.Time `gorm:"index"`
	UserID    string    `gorm:"size:128;index"`
	Action    string    `gorm:"size:128;not null"`
	Module    string    `gorm:"size:128"`
	Result    string    `gorm:"size:32;not null"`
	Metadata  []byte    `gorm:"type:text"`
}

func (AuditLog) TableName() string { return "audit_log" }

// ModuleState tracks per-app module configuration and runtime state.
type ModuleState struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement"`
	ModuleID   string `gorm:"size:128;not null;uniqueIndex:idx_modstate_module_app,priority:1"`
	AppID      string `gorm:"size:128;not null;uniqueIndex:idx_modstate_module_app,priority:2"`
	State      string `gorm:"size:32;not null"`
	Config     []byte `gorm:"type:text"`
	LastUpdate time.Time
}

func (ModuleState) TableName() string { return "module_state" }
