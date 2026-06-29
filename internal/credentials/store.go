// Package credentials implements the user's per-user encrypted credential
// vault: third-party provider secrets (LLM API keys, database URLs, OAuth
// tokens, …) the user stores so their own apps/agents can use them.
//
// Scope is ALWAYS per-user — there is no app/system scope and no grant table.
// A credential belongs to a user and only that user's sessions resolve it.
//
// The settings-plane CRUD here is physically separate from the agent runtime:
// it touches the DB + cipher only on explicit user actions, never on a turn's
// hot path. Plaintext secrets never leave the daemon and are never returned by
// the read API — callers only ever see the masked previews in DisplayMeta.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// ErrNotFound is returned when a credential id is absent or owned by another user.
var ErrNotFound = errors.New("credentials: not found")

// Sealer is the subset of mcpoauth.Sealer the vault needs. Declared here so the
// package depends on a behaviour, not on the server subpackage.
type Sealer interface {
	Seal(plaintext []byte) (string, error)
	Open(encoded string) ([]byte, error)
}

// Store is the gorm-backed vault. All methods are scoped by userID; a row is
// only ever read, written, or deleted for its owner.
type Store struct {
	db      *gorm.DB
	sealer  Sealer
	copilot *copilotFlows
	models  *modelsCache
}

func NewStore(db *gorm.DB, sealer Sealer) *Store {
	return &Store{db: db, sealer: sealer, copilot: newCopilotFlows(), models: newModelsCache()}
}

// listRows returns all of a user's credential rows (sealed payload included) —
// INTERNAL, for model listing / runtime resolution.
func (s *Store) listRows(ctx context.Context, userID string) ([]models.UserCredential, error) {
	var rows []models.UserCredential
	if err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("provider_name").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// openFields decrypts a sealed payload into its fields map. INTERNAL.
func (s *Store) openFields(sealed string) (map[string]string, error) {
	plain, err := s.sealer.Open(sealed)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	if len(plain) > 0 {
		if err := json.Unmarshal(plain, &fields); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

// CredView is the secret-free projection returned by the read API. It carries
// only the masked previews (FieldsMasked) — never plaintext field values.
type CredView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	ProviderName string            `json:"provider_name"`
	ProviderType string            `json:"provider_type"`
	Scope        string            `json:"scope"` // always "per_user"
	Status       string            `json:"status"`
	Label        string            `json:"label"`
	FieldsMasked map[string]string `json:"fields_masked"`
	ExpiresAt    *time.Time        `json:"expires_at"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// CreateInput is the plaintext payload from POST /api/credentials. Fields holds
// the raw secret values; they are sealed before persistence and dropped.
type CreateInput struct {
	ProviderName string
	ProviderType string
	Label        string
	Name         string
	Fields       map[string]string
}

// List returns all of the user's credentials, newest first, without secrets.
func (s *Store) List(ctx context.Context, userID string) ([]CredView, error) {
	var rows []models.UserCredential
	if err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]CredView, 0, len(rows))
	for i := range rows {
		out = append(out, rowToView(&rows[i]))
	}
	return out, nil
}

// Get returns one credential by id, scoped to the owner. ErrNotFound otherwise.
func (s *Store) Get(ctx context.Context, userID, id string) (*CredView, error) {
	row, err := s.find(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	v := rowToView(row)
	return &v, nil
}

// Create seals the supplied fields and persists a new per-user credential.
func (s *Store) Create(ctx context.Context, userID string, in CreateInput) (*CredView, error) {
	fields := trimValues(in.Fields)
	sealed, err := s.sealer.Seal(marshalFields(fields))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := models.UserCredential{
		ID:           uuid.NewString(),
		UserID:       userID,
		ProviderName: in.ProviderName,
		ProviderType: orDefault(in.ProviderType, "api_key"),
		Name:         in.Name,
		Label:        orDefault(in.Label, "default"),
		Sealed:       sealed,
		DisplayMeta:  marshalMeta(fields),
		Status:       "valid",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	v := rowToView(&row)
	return &v, nil
}

// Update changes the label and/or re-seals the fields of an owned credential.
func (s *Store) Update(ctx context.Context, userID, id string, label *string, fields map[string]string) (*CredView, error) {
	row, err := s.find(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if label != nil {
		row.Label = *label
	}
	if len(fields) > 0 {
		fields = trimValues(fields)
		sealed, serr := s.sealer.Seal(marshalFields(fields))
		if serr != nil {
			return nil, serr
		}
		row.Sealed = sealed
		row.DisplayMeta = marshalMeta(fields)
		row.Status = "valid"
	}
	row.UpdatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		return nil, err
	}
	v := rowToView(row)
	return &v, nil
}

// Delete removes an owned credential. ErrNotFound when it doesn't exist for the user.
func (s *Store) Delete(ctx context.Context, userID, id string) error {
	res := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", id, userID).
		Delete(&models.UserCredential{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// reveal returns the decrypted fields of an owned credential. INTERNAL ONLY —
// used by live verification (refresh) and, later, runtime resolution. The
// plaintext is never exposed through the HTTP API.
func (s *Store) reveal(ctx context.Context, userID, id string) (providerName string, fields map[string]string, err error) {
	row, err := s.find(ctx, userID, id)
	if err != nil {
		return "", nil, err
	}
	plain, err := s.sealer.Open(row.Sealed)
	if err != nil {
		return "", nil, err
	}
	fields = map[string]string{}
	if len(plain) > 0 {
		if err := json.Unmarshal(plain, &fields); err != nil {
			return "", nil, err
		}
	}
	return row.ProviderName, fields, nil
}

// Verify runs a live test against the provider for an existing credential and
// records the outcome (status + last_validated_at). Returns the test result.
func (s *Store) Verify(ctx context.Context, userID, id string) (TestResult, error) {
	provider, fields, err := s.reveal(ctx, userID, id)
	if err != nil {
		return TestResult{}, err
	}
	res := RunTest(ctx, provider, fields)
	status := "valid"
	if !res.OK {
		// A "not available" result must not flip a credential to invalid.
		if strings.Contains(res.Detail, "not available") {
			return res, nil
		}
		status = "invalid"
	}
	now := time.Now().UTC()
	s.db.WithContext(ctx).
		Model(&models.UserCredential{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{"status": status, "last_validated_at": now, "updated_at": now})
	return res, nil
}

func (s *Store) find(ctx context.Context, userID, id string) (*models.UserCredential, error) {
	var row models.UserCredential
	err := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", id, userID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func rowToView(row *models.UserCredential) CredView {
	return CredView{
		ID:           row.ID,
		Name:         row.Name,
		ProviderName: row.ProviderName,
		ProviderType: row.ProviderType,
		Scope:        "per_user",
		Status:       row.Status,
		Label:        row.Label,
		FieldsMasked: maskedFromMeta(row.DisplayMeta),
		ExpiresAt:    row.ExpiresAt,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}

// trimValues strips surrounding whitespace from every field value. Pasted
// secrets routinely carry a trailing newline/space that breaks auth headers
// (e.g. a 401 "invalid x-api-key" on an otherwise valid key); leading/trailing
// whitespace is never meaningful in a credential value.
func trimValues(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[k] = strings.TrimSpace(v)
	}
	return out
}

func marshalFields(fields map[string]string) []byte {
	if fields == nil {
		fields = map[string]string{}
	}
	b, _ := json.Marshal(fields)
	return b
}

func marshalMeta(fields map[string]string) []byte {
	b, _ := json.Marshal(map[string]any{"masked_fields": maskFields(fields)})
	return b
}

func maskedFromMeta(meta []byte) map[string]string {
	out := map[string]string{}
	if len(meta) == 0 {
		return out
	}
	var parsed struct {
		MaskedFields map[string]string `json:"masked_fields"`
	}
	if err := json.Unmarshal(meta, &parsed); err == nil && parsed.MaskedFields != nil {
		return parsed.MaskedFields
	}
	return out
}

func maskFields(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[k] = mask(v)
	}
	return out
}

// mask keeps the first 3 and last 4 runes around an ellipsis for a recognisable
// preview; short values are fully bulleted so nothing meaningful leaks.
func mask(v string) string {
	r := []rune(strings.TrimSpace(v))
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 8 {
		return strings.Repeat("•", len(r))
	}
	return string(r[:3]) + "…" + string(r[len(r)-4:])
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
