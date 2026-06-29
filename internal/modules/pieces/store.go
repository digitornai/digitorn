package pieces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
	"github.com/mbathepaul/digitorn/internal/server/mcpoauth"
	"gorm.io/gorm"
)

// canonicalPieceName normalises a connector id the same way the bridge does
// when loading bundles (lowercase, hyphens to underscores). Credentials are
// keyed by this canonical id so a connector stored from the hub catalog
// ("telegram-bot") is found when the agent calls its tool, whose piece id is
// the bridge's canonical form ("telegram_bot"). Without this, multi-word
// connectors store and reveal under different keys and auth is never injected.
func canonicalPieceName(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", "_"))
}

// Store manages per-user installed piece credentials (sealed at rest).
type Store struct {
	db     *gorm.DB
	sealer *mcpoauth.Sealer
}

// InstalledPieceView is the redacted view returned to callers (no raw secrets).
type InstalledPieceView struct {
	UserID    string
	PieceName string
	Version   string
	AuthType  string
	SecretKeys []string // names only, no values
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// APAuthWire is the _ap_auth format the bridge expects.
type APAuthWire struct {
	Type         string            `json:"type"`
	Value        string            `json:"value,omitempty"`
	Fields       map[string]string `json:"fields,omitempty"`
	AccessToken  string            `json:"accessToken,omitempty"`
	TokenType    string            `json:"tokenType,omitempty"`
	ExpiresAt    int64             `json:"expiresAt,omitempty"`
	RefreshToken string            `json:"refreshToken,omitempty"`
	Scope        string            `json:"scope,omitempty"`
	Username     string            `json:"username,omitempty"`
	Password     string            `json:"password,omitempty"`
}

func newStore(db *gorm.DB, sealer *mcpoauth.Sealer) *Store {
	return &Store{db: db, sealer: sealer}
}

// Install creates or replaces a user's piece credentials.
func (s *Store) Install(ctx context.Context, userID, pieceName, version, authType string, creds map[string]string) error {
	if userID == "" || pieceName == "" {
		return errors.New("userID and pieceName are required")
	}
	pieceName = canonicalPieceName(pieceName)
	sealed, err := s.seal(creds)
	if err != nil {
		return fmt.Errorf("seal credentials: %w", err)
	}
	row := models.InstalledPiece{
		UserID:     userID,
		PieceName:  pieceName,
		Version:    version,
		AuthType:   authType,
		SealedAuth: sealed,
		Enabled:    true,
	}
	return s.db.WithContext(ctx).
		Where(models.InstalledPiece{UserID: userID, PieceName: pieceName}).
		Assign(row).
		FirstOrCreate(&row).Error
}

// Get returns the view for a piece (no raw secrets).
func (s *Store) Get(ctx context.Context, userID, pieceName string) (*InstalledPieceView, bool, error) {
	pieceName = canonicalPieceName(pieceName)
	var row models.InstalledPiece
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND piece_name = ?", userID, pieceName).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return toView(row, s.secretKeys(row)), true, nil
}

// List returns all installed pieces for a user.
func (s *Store) List(ctx context.Context, userID string) ([]InstalledPieceView, error) {
	var rows []models.InstalledPiece
	if err := s.db.WithContext(ctx).Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]InstalledPieceView, len(rows))
	for i, r := range rows {
		out[i] = *toView(r, s.secretKeys(r))
	}
	return out, nil
}

// Update replaces credentials for an existing piece.
func (s *Store) Update(ctx context.Context, userID, pieceName string, creds map[string]string) error {
	pieceName = canonicalPieceName(pieceName)
	sealed, err := s.seal(creds)
	if err != nil {
		return fmt.Errorf("seal credentials: %w", err)
	}
	res := s.db.WithContext(ctx).
		Model(&models.InstalledPiece{}).
		Where("user_id = ? AND piece_name = ?", userID, pieceName).
		Update("sealed_auth", sealed)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("piece %q not installed for user %q", pieceName, userID)
	}
	return nil
}

// Delete removes an installed piece.
func (s *Store) Delete(ctx context.Context, userID, pieceName string) error {
	pieceName = canonicalPieceName(pieceName)
	return s.db.WithContext(ctx).
		Where("user_id = ? AND piece_name = ?", userID, pieceName).
		Delete(&models.InstalledPiece{}).Error
}

// UpsertOAuth stores OAuth2 credentials for a piece.
func (s *Store) UpsertOAuth(ctx context.Context, userID, pieceName, accessToken, refreshToken, tokenType string, expiresAt int64, scope string) error {
	pieceName = canonicalPieceName(pieceName)
	creds := map[string]string{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"token_type":    tokenType,
		"scope":         scope,
	}
	if expiresAt > 0 {
		creds["expires_at"] = fmt.Sprintf("%d", expiresAt)
	}
	sealed, err := s.seal(creds)
	if err != nil {
		return fmt.Errorf("seal credentials: %w", err)
	}
	row := models.InstalledPiece{
		UserID:     userID,
		PieceName:  pieceName,
		AuthType:   "oauth2",
		SealedAuth: sealed,
		Enabled:    true,
	}
	return s.db.WithContext(ctx).
		Where(models.InstalledPiece{UserID: userID, PieceName: pieceName}).
		Assign(row).
		FirstOrCreate(&row).Error
}

// RevealAuth unseals and returns the _ap_auth wire object for the bridge.
func (s *Store) RevealAuth(ctx context.Context, userID, pieceName string) (*APAuthWire, error) {
	pieceName = canonicalPieceName(pieceName)
	var row models.InstalledPiece
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND piece_name = ?", userID, pieceName).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("piece %q not installed", pieceName)
	}
	if err != nil {
		return nil, err
	}
	creds, err := s.unseal(row.SealedAuth)
	if err != nil {
		return nil, fmt.Errorf("unseal credentials: %w", err)
	}
	return credsToWire(row.AuthType, creds), nil
}

func credsToWire(authType string, creds map[string]string) *APAuthWire {
	switch authType {
	case "secret_text":
		return &APAuthWire{Type: "secret_text", Value: creds["value"]}
	case "basic":
		return &APAuthWire{Type: "basic", Username: creds["username"], Password: creds["password"]}
	case "oauth2":
		var expiresAt int64
		if v := creds["expires_at"]; v != "" {
			fmt.Sscan(v, &expiresAt)
		}
		return &APAuthWire{
			Type:         "oauth2",
			AccessToken:  creds["access_token"],
			TokenType:    creds["token_type"],
			ExpiresAt:    expiresAt,
			RefreshToken: creds["refresh_token"],
			Scope:        creds["scope"],
		}
	case "custom":
		fields := make(map[string]string, len(creds))
		maps.Copy(fields, creds)
		return &APAuthWire{Type: "custom", Fields: fields}
	default:
		return &APAuthWire{Type: "none"}
	}
}

func (s *Store) secretKeys(row models.InstalledPiece) []string {
	if len(row.SealedAuth) == 0 {
		return nil
	}
	creds, err := s.unseal(row.SealedAuth)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(creds))
	for k := range creds {
		keys = append(keys, k)
	}
	return keys
}

func (s *Store) seal(creds map[string]string) ([]byte, error) {
	raw, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	if s.sealer == nil {
		return raw, nil
	}
	enc, err := s.sealer.Seal(raw)
	if err != nil {
		return nil, err
	}
	return []byte(enc), nil
}

func (s *Store) unseal(data []byte) (map[string]string, error) {
	if len(data) == 0 {
		return map[string]string{}, nil
	}
	var raw []byte
	if s.sealer != nil {
		var err error
		raw, err = s.sealer.Open(string(data))
		if err != nil {
			return nil, err
		}
	} else {
		raw = data
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func toView(row models.InstalledPiece, secretKeys []string) *InstalledPieceView {
	return &InstalledPieceView{
		UserID:    row.UserID,
		PieceName: row.PieceName,
		Version:   row.Version,
		AuthType:  row.AuthType,
		SecretKeys: secretKeys,
		Enabled:   row.Enabled,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
