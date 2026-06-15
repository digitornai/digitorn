// Package mcpservers is the per-user managed MCP server store. A managed server
// is one a user installed once (from the catalog, the registry, or a raw spec)
// and reuses across their apps — an app opts in by referencing the server id.
// It lives in the daemon's metadata DB (NOT an app bundle) so it survives app
// upgrades, and is layered into an app's MCP config per user at runtime. Secret
// VALUES (tokens, api keys) are sealed at rest; the API view never returns them.
package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
	"github.com/mbathepaul/digitorn/internal/server/mcpoauth"
)

var (
	// ErrNotFound : no managed server matches the (user, server id) pair.
	ErrNotFound = errors.New("mcpservers: server not found")
	// ErrConflict : the user already installed a server under this id.
	ErrConflict = errors.New("mcpservers: a server with this id already exists")
	// ErrInvalidID : the server id is not a valid slug.
	ErrInvalidID = errors.New("mcpservers: invalid server id")
	// ErrInvalidSpec : the spec is internally inconsistent (e.g. stdio without a command).
	ErrInvalidSpec = errors.New("mcpservers: invalid server spec")
)

// idRE is the server-id slug rule: 1..128 chars, lowercase letters/digits and
// -_ separators, starting with a letter or digit.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// NormalizeID trims + lower-cases a server id, then validates the slug.
func NormalizeID(id string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(id))
	if !idRE.MatchString(n) {
		return "", ErrInvalidID
	}
	return n, nil
}

// Spec is the writable shape of a managed server (install + update). Secrets are
// the plaintext credential values keyed by env-var name; they are sealed by the
// store and never read back through the API.
type Spec struct {
	ServerID    string
	DisplayName string
	Source      string // catalog | registry | custom
	Transport   string // stdio | streamable_http | sse
	Command     string
	Args        []string
	URL         string
	Env         map[string]string // non-secret env
	Secrets     map[string]string // sealed at rest
	AuthType    string            // "" | oauth2 | token
	Package     string
}

// ManagedServer is the store's read view — secrets are redacted to their key
// names only (SecretKeys), never their values.
type ManagedServer struct {
	ID          string            `json:"id"`
	UserID      string            `json:"user_id"`
	ServerID    string            `json:"server_id"`
	DisplayName string            `json:"display_name"`
	Source      string            `json:"source"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	URL         string            `json:"url,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	SecretKeys  []string          `json:"secret_keys"`
	AuthType    string            `json:"auth_type,omitempty"`
	Package     string            `json:"package,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Store is the GORM-backed CRUD over managed MCP servers, sealing secret values.
type Store struct {
	db     *gorm.DB
	sealer *mcpoauth.Sealer
}

// NewStore binds a Store to the metadata DB and the process sealer. The sealer
// must be non-nil — managed servers may carry sealed secrets.
func NewStore(db *gorm.DB, sealer *mcpoauth.Sealer) *Store {
	return &Store{db: db, sealer: sealer}
}

func (s *Store) secretKeys(sealed []byte) []string {
	m, err := s.openSecrets(sealed)
	if err != nil || len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Store) toView(r models.ManagedMCPServer) ManagedServer {
	return ManagedServer{
		ID: r.ID, UserID: r.UserID, ServerID: r.ServerID, DisplayName: r.DisplayName,
		Source: r.Source, Transport: r.Transport, Command: r.Command, Args: r.Args,
		URL: r.URL, Env: r.Env, SecretKeys: s.secretKeys(r.Secrets), AuthType: r.AuthType,
		Package: r.Package, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s *Store) sealSecrets(secrets map[string]string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	enc, err := s.sealer.Seal(raw)
	if err != nil {
		return nil, err
	}
	return []byte(enc), nil
}

func (s *Store) openSecrets(sealed []byte) (map[string]string, error) {
	if len(sealed) == 0 {
		return map[string]string{}, nil
	}
	raw, err := s.sealer.Open(string(sealed))
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// validate normalizes + checks a spec, returning the normalized server id.
func validate(spec *Spec) (string, error) {
	id, err := NormalizeID(spec.ServerID)
	if err != nil {
		return "", err
	}
	transport := strings.TrimSpace(spec.Transport)
	if transport == "" {
		transport = "stdio"
	}
	switch transport {
	case "stdio":
		if strings.TrimSpace(spec.Command) == "" {
			return "", ErrInvalidSpec // stdio needs a command
		}
	case "streamable_http", "sse", "http":
		if strings.TrimSpace(spec.URL) == "" {
			return "", ErrInvalidSpec // remote needs a url
		}
		if transport == "http" {
			transport = "streamable_http"
		}
	default:
		return "", ErrInvalidSpec
	}
	spec.ServerID = id
	spec.Transport = transport
	return id, nil
}

// Install persists a new managed server for the user. Returns ErrConflict when
// the user already installed a server under this id.
func (s *Store) Install(ctx context.Context, userID string, spec Spec) (ManagedServer, error) {
	id, err := validate(&spec)
	if err != nil {
		return ManagedServer{}, err
	}
	taken, err := s.exists(ctx, userID, id)
	if err != nil {
		return ManagedServer{}, err
	}
	if taken {
		return ManagedServer{}, ErrConflict
	}
	sealed, err := s.sealSecrets(spec.Secrets)
	if err != nil {
		return ManagedServer{}, err
	}
	now := time.Now().UTC()
	row := models.ManagedMCPServer{
		ID: uuid.NewString(), UserID: userID, ServerID: id,
		DisplayName: strings.TrimSpace(spec.DisplayName), Source: spec.Source,
		Transport: spec.Transport, Command: strings.TrimSpace(spec.Command), Args: spec.Args,
		URL: strings.TrimSpace(spec.URL), Env: spec.Env, Secrets: sealed,
		AuthType: spec.AuthType, Package: spec.Package, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return ManagedServer{}, err
	}
	return s.toView(row), nil
}

// List returns the user's managed servers, most-recently-updated first.
func (s *Store) List(ctx context.Context, userID string) ([]ManagedServer, error) {
	var rows []models.ManagedMCPServer
	if err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ManagedServer, len(rows))
	for i := range rows {
		out[i] = s.toView(rows[i])
	}
	return out, nil
}

// Get resolves one managed server by id within the user. found is false (nil
// error) when nothing matches.
func (s *Store) Get(ctx context.Context, userID, serverID string) (ManagedServer, bool, error) {
	row, found, err := s.row(ctx, userID, serverID)
	if err != nil || !found {
		return ManagedServer{}, found, err
	}
	return s.toView(row), true, nil
}

// Patch is a partial update; nil pointers leave a field untouched. Env and
// Secrets, when non-nil, REPLACE the stored maps (secrets are re-sealed).
type Patch struct {
	DisplayName *string
	Transport   *string
	Command     *string
	Args        *[]string
	URL         *string
	Env         *map[string]string
	Secrets     *map[string]string
	AuthType    *string
}

// Update applies a partial change to the user's server. Returns ErrNotFound when
// the server isn't the caller's, ErrInvalidSpec when the result is inconsistent.
func (s *Store) Update(ctx context.Context, userID, serverID string, p Patch) (ManagedServer, error) {
	row, found, err := s.row(ctx, userID, serverID)
	if err != nil {
		return ManagedServer{}, err
	}
	if !found {
		return ManagedServer{}, ErrNotFound
	}
	if p.DisplayName != nil {
		row.DisplayName = strings.TrimSpace(*p.DisplayName)
	}
	if p.Transport != nil {
		row.Transport = strings.TrimSpace(*p.Transport)
	}
	if p.Command != nil {
		row.Command = strings.TrimSpace(*p.Command)
	}
	if p.Args != nil {
		row.Args = *p.Args
	}
	if p.URL != nil {
		row.URL = strings.TrimSpace(*p.URL)
	}
	if p.Env != nil {
		row.Env = *p.Env
	}
	if p.AuthType != nil {
		row.AuthType = strings.TrimSpace(*p.AuthType)
	}
	if p.Secrets != nil {
		sealed, err := s.sealSecrets(*p.Secrets)
		if err != nil {
			return ManagedServer{}, err
		}
		row.Secrets = sealed
	}
	// Re-validate the resulting transport/command/url coherence.
	check := Spec{ServerID: row.ServerID, Transport: row.Transport, Command: row.Command, URL: row.URL}
	if _, err := validate(&check); err != nil {
		return ManagedServer{}, err
	}
	row.Transport = check.Transport
	row.UpdatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return ManagedServer{}, err
	}
	return s.toView(row), nil
}

// Delete hard-deletes the user's server. Returns ErrNotFound when nothing
// matched — never another user's row.
func (s *Store) Delete(ctx context.Context, userID, serverID string) error {
	id, err := NormalizeID(serverID)
	if err != nil {
		return ErrNotFound
	}
	res := s.db.WithContext(ctx).
		Where("user_id = ? AND server_id = ?", userID, id).
		Delete(&models.ManagedMCPServer{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Reveal returns a server's view together with its UNSEALED secret values. Used
// only by the runtime layering and the connectivity test — never by the read
// API. found is false (nil error) when nothing matches.
func (s *Store) Reveal(ctx context.Context, userID, serverID string) (ManagedServer, map[string]string, bool, error) {
	row, found, err := s.row(ctx, userID, serverID)
	if err != nil || !found {
		return ManagedServer{}, nil, found, err
	}
	secrets, err := s.openSecrets(row.Secrets)
	if err != nil {
		return ManagedServer{}, nil, true, err
	}
	return s.toView(row), secrets, true, nil
}

func (s *Store) row(ctx context.Context, userID, serverID string) (models.ManagedMCPServer, bool, error) {
	id, err := NormalizeID(serverID)
	if err != nil {
		return models.ManagedMCPServer{}, false, nil
	}
	var row models.ManagedMCPServer
	err = s.db.WithContext(ctx).
		Where("user_id = ? AND server_id = ?", userID, id).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.ManagedMCPServer{}, false, nil
	}
	if err != nil {
		return models.ManagedMCPServer{}, false, err
	}
	return row, true, nil
}

func (s *Store) exists(ctx context.Context, userID, serverID string) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.ManagedMCPServer{}).
		Where("user_id = ? AND server_id = ?", userID, serverID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
