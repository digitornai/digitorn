package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// Token is the decrypted OAuth token bundle. Access AND refresh are encrypted
// together at rest, keyed by (user, provider).
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // unix seconds; 0 = no expiry
	Scope        string `json:"scope,omitempty"`
}

// Store persists encrypted OAuth tokens over the shared credentials table.
type Store struct {
	db     *gorm.DB
	sealer *Sealer
}

func NewStore(db *gorm.DB, sealer *Sealer) *Store {
	return &Store{db: db, sealer: sealer}
}

// Get returns the (userID, provider) token, or (nil, nil) when none exists.
func (s *Store) Get(ctx context.Context, userID, provider string) (*Token, error) {
	var row models.Credential
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND provider = ?", userID, provider).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.sealer.Open(string(row.Fields))
	if err != nil {
		return nil, err
	}
	var tok Token
	if err := json.Unmarshal(plain, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// Set upserts the (userID, provider) token, encrypted at rest.
func (s *Store) Set(ctx context.Context, userID, provider string, tok *Token) error {
	plain, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	sealed, err := s.sealer.Seal(plain)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row := models.Credential{
		ID:        uuid.New(),
		UserID:    userID,
		Provider:  provider,
		Fields:    []byte(sealed),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "provider"}},
		DoUpdates: clause.AssignmentColumns([]string{"fields", "updated_at"}),
	}).Create(&row).Error
}

// Delete removes ONLY the (userID, provider) token (never other users' rows).
func (s *Store) Delete(ctx context.Context, userID, provider string) error {
	return s.db.WithContext(ctx).
		Where("user_id = ? AND provider = ?", userID, provider).
		Delete(&models.Credential{}).Error
}
