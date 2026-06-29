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
	Category  string `gorm:"size:64;index"`
	Author    string `gorm:"size:128"`
	ShortName string `gorm:"size:64"`  // optional compact label (e.g. "Claude Pro")
	Icon      string `gorm:"size:256"` // file path (icon.png) OR text/emoji (🧱)
	Color     string `gorm:"size:16"`  // hex color, e.g. #14B8A6 — used when Icon is text/emoji

	// DisplayName is a user-set override for the displayed label. When set it
	// wins over the bundle's ShortName and, unlike ShortName, is NOT overwritten
	// on reload/upgrade. Empty = fall back to ShortName. Set via
	// PUT /api/apps/{app_id}/display-name.
	DisplayName string `gorm:"size:64"`

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

// UserCredential is one entry in the user's encrypted credential vault — a
// third-party provider secret (LLM API key, database URL, OAuth token, …) the
// user stores so their own apps/agents can use it. Scope is ALWAYS per-user:
// there is no app/system scope and no grant table — a credential belongs to a
// user and that user's own sessions resolve it. The secret payload lives sealed
// in Sealed (mcpoauth.Sealer over the JSON fields map); the plaintext NEVER
// leaves the daemon and is NEVER returned by the read API — the UI shows only
// the masked previews kept in DisplayMeta. Distinct from the shared
// `credentials` table (mcpoauth tokens) on purpose: a dedicated table keeps the
// vault schema independent and avoids drift on the shared one.
type UserCredential struct {
	ID           string `gorm:"size:64;primaryKey"`
	UserID       string `gorm:"size:128;not null;index:idx_ucred_user_provider,priority:1"`
	ProviderName string `gorm:"size:128;not null;index:idx_ucred_user_provider,priority:2"`
	ProviderType string `gorm:"size:32;not null"` // api_key, oauth2, connection_string, …
	Name         string `gorm:"size:64;not null"` // optional stable slug for YAML refs
	Label        string `gorm:"size:64;not null"` // human label for the picker
	Sealed       string `gorm:"type:text"`        // Sealer.Seal(JSON(fields))
	DisplayMeta  []byte `gorm:"type:text"`        // JSON {"masked_fields": {...}}
	Status       string `gorm:"size:32;not null"` // valid, expired, invalid, …

	ExpiresAt       *time.Time
	LastValidatedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (UserCredential) TableName() string { return "user_credentials" }

// UserModuleConfig is one user's per-app, per-module config DELTAS (BYOK mode):
// only the fields the user changed, sealed at rest. The YAML `config:` block is
// the immutable per-app default; when the app's BYOK flag is on, these deltas
// deep-merge over it for that user. Keyed (user_id, app_id, module_id).
type UserModuleConfig struct {
	ID        string `gorm:"size:64;primaryKey"`
	UserID    string `gorm:"size:128;not null;uniqueIndex:idx_umodcfg_user_app_module,priority:1"`
	AppID     string `gorm:"size:128;not null;uniqueIndex:idx_umodcfg_user_app_module,priority:2"`
	ModuleID  string `gorm:"size:128;not null;uniqueIndex:idx_umodcfg_user_app_module,priority:3"`
	Sealed    string `gorm:"type:text"` // Sealer.Seal(JSON(deltas))
	UpdatedAt time.Time
}

func (UserModuleConfig) TableName() string { return "user_module_config" }

// UserAppSecret is one user's per-app secret value (a channel bot token, an API
// key referenced as `{{secret.X}}` in the bundle), sealed at rest. Keyed
// (user_id, app_id, key). Set via PUT /api/apps/{id}/secrets; resolved by the
// background service through the daemon at channel-arm time.
type UserAppSecret struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    string    `gorm:"size:128;not null;uniqueIndex:idx_uappsec_user_app_key,priority:1"`
	AppID     string    `gorm:"size:128;not null;uniqueIndex:idx_uappsec_user_app_key,priority:2"`
	Key       string    `gorm:"size:128;not null;uniqueIndex:idx_uappsec_user_app_key,priority:3"`
	Sealed    string    `gorm:"type:text"` // Sealer.Seal(value)
	UpdatedAt time.Time
}

func (UserAppSecret) TableName() string { return "user_app_secret" }

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

// OAuthClient is a client dynamically registered (RFC 7591) with one OAuth
// authorization server, keyed by issuer. It is reused across users, apps and
// daemon restarts so dynamic registration runs once per authorization server.
// ClientSecret is sealed at rest (empty for public clients).
type OAuthClient struct {
	Issuer          string `gorm:"size:512;primaryKey"`
	ClientID        string `gorm:"size:512;not null"`
	ClientSecret    []byte `gorm:"type:text"`
	RegistrationURI string `gorm:"size:512"`
	Metadata        []byte `gorm:"type:text"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (OAuthClient) TableName() string { return "oauth_clients" }

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

// ManagedMCPServer is an MCP server a user installed once and reuses across
// their apps (an app opts in by referencing the server id). It lives in the
// daemon's metadata DB — NOT an app bundle — so it survives app upgrades and is
// layered into an app's MCP config per user at runtime. The (user_id, server_id)
// pair is unique : a user can't shadow their own server id, but two users hold
// independent servers under the same id. Secrets (token / api-key VALUES) are
// sealed at rest; Env holds only non-secret config (hosts, ports, flags).
type ManagedMCPServer struct {
	ID          string            `gorm:"size:64;primaryKey"`
	UserID      string            `gorm:"size:128;not null;uniqueIndex:idx_mcpsrv_user_server,priority:1"`
	ServerID    string            `gorm:"size:128;not null;uniqueIndex:idx_mcpsrv_user_server,priority:2"`
	DisplayName string            `gorm:"size:200;not null;default:''"`
	Source      string            `gorm:"size:32;not null;default:''"` // catalog | registry | custom
	Transport   string            `gorm:"size:32;not null;default:'stdio'"`
	Command     string            `gorm:"size:512"`
	Args        []string          `gorm:"serializer:json"`
	URL         string            `gorm:"size:1024"`
	Env         map[string]string `gorm:"serializer:json"`             // non-secret env (IMAP_HOST, ports, flags)
	Secrets     []byte            `gorm:"type:text"`                   // sealed JSON map[name]value (token / api-key values)
	AuthType    string            `gorm:"size:32;not null;default:''"` // "" | oauth2 | token
	Package     string            `gorm:"size:256"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (ManagedMCPServer) TableName() string { return "managed_mcp_servers" }

// InstalledPiece is a per-user Activepieces connector with its sealed
// credentials. The (user_id, piece_name) pair is unique — one credential set
// per piece per user, shared across all apps the user uses the piece in.
// SealedAuth stores the JSON-marshalled credential map, sealed with mcpoauth.Sealer.
type InstalledPiece struct {
	ID         uint   `gorm:"primaryKey;autoIncrement"`
	UserID     string `gorm:"size:128;not null;uniqueIndex:idx_installed_piece_user_name,priority:1"`
	PieceName  string `gorm:"size:128;not null;uniqueIndex:idx_installed_piece_user_name,priority:2"`
	Version    string `gorm:"size:64;not null;default:''"`
	AuthType   string `gorm:"size:32;not null;default:'none'"` // secret_text|custom|oauth2|basic|none
	SealedAuth []byte `gorm:"type:text"`
	Enabled    bool   `gorm:"not null;default:true"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (InstalledPiece) TableName() string { return "installed_pieces" }
