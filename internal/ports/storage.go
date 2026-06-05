package ports

import "context"

// CredentialRepository persists encrypted user credentials (API keys, OAuth tokens).
type CredentialRepository interface {
	Get(ctx context.Context, userID, provider string) (map[string]any, error)
	Set(ctx context.Context, userID, provider string, fields map[string]any) error
	Delete(ctx context.Context, userID, provider string) error
}

// AuditRepository persists audit log entries.
type AuditRepository interface {
	Append(ctx context.Context, entry AuditEntry) error
}

// AuditEntry is one audit log row.
type AuditEntry struct {
	Timestamp int64          `json:"timestamp"`
	UserID    string         `json:"user_id,omitempty"`
	Action    string         `json:"action"`
	Module    string         `json:"module,omitempty"`
	Result    string         `json:"result"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}
