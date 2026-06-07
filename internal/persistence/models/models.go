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

// OAuthState is a pending MCP OAuth authorization, binding a CSRF state token to
// the user who started the flow (the provider callback can't carry the JWT).
// Single-use (deleted on read) and short-lived (ExpiresAt). The PKCE Verifier is
// a secret, stored encrypted.
type OAuthState struct {
	State       string    `gorm:"size:128;primaryKey"`
	UserID      string    `gorm:"size:128;not null;index"`
	AppID       string    `gorm:"size:128;not null"`
	Provider    string    `gorm:"size:128;not null"`
	ServerID    string    `gorm:"size:128;not null"`
	Verifier    []byte    `gorm:"type:text"`
	Nonce       string    `gorm:"size:128"`
	RedirectURI string    `gorm:"size:512"`
	ExpiresAt   time.Time `gorm:"index"`
	CreatedAt   time.Time
}

func (OAuthState) TableName() string { return "oauth_state" }

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

// UserSkill is a per-user, per-app authored skill : a named slash-command
// directive the agent can load via use_skill, alongside the app's own bundled
// skills. Owned by the user, scoped to one app, and persisted here (not in the
// app bundle) so it survives app upgrades — the bundle dir is wiped and rebuilt
// on every (re)install. The (user_id, app_id, name) triple is unique : one user
// can't shadow their own skill name within an app, but two users hold
// independent skills for the same app.
type UserSkill struct {
	ID           string `gorm:"size:64;primaryKey"`
	UserID       string `gorm:"size:128;not null;uniqueIndex:idx_uskill_user_app_name,priority:1"`
	AppID        string `gorm:"size:128;not null;uniqueIndex:idx_uskill_user_app_name,priority:2"`
	Name         string `gorm:"size:64;not null;uniqueIndex:idx_uskill_user_app_name,priority:3"`
	Description  string `gorm:"size:300;not null;default:''"`
	Instructions string `gorm:"type:text;not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (UserSkill) TableName() string { return "user_skills" }

// UserSnippet is a per-user, per-app saved prompt the chat composer can insert.
// Unlike a skill it is NEVER consumed by the agent/runtime — it's a pure
// client-side convenience (title + body, with an optional emoji + tags). Stored
// here (not the bundle) so it survives app upgrades. No name uniqueness : a user
// may keep several snippets with the same title.
type UserSnippet struct {
	ID        string   `gorm:"size:64;primaryKey"`
	UserID    string   `gorm:"size:128;not null;index:idx_usnip_user_app,priority:1"`
	AppID     string   `gorm:"size:128;not null;index:idx_usnip_user_app,priority:2"`
	Title     string   `gorm:"size:200;not null"`
	Body      string   `gorm:"type:text;not null"`
	Emoji     string   `gorm:"size:16;not null;default:''"`
	Tags      []string `gorm:"serializer:json"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (UserSnippet) TableName() string { return "user_snippets" }
