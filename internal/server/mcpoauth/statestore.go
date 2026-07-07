package mcpoauth

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

// stateTTL bounds how long a pending authorization may sit before its callback.
const stateTTL = 10 * time.Minute

// PendingState is one in-flight authorization, bound to the user who started it.
type PendingState struct {
	State        string
	UserID       string
	AppID        string
	Provider     string
	ServerID     string
	Verifier     string
	Nonce        string
	RedirectURI  string
	ClientID     string // optional: OAuth client ID for pieces
	ClientSecret string // optional: OAuth client secret for pieces
}

// StateStore persists CSRF state rows, encrypting the PKCE verifier at rest.
type StateStore struct {
	db     *gorm.DB
	sealer *Sealer
}

func NewStateStore(db *gorm.DB, sealer *Sealer) *StateStore {
	return &StateStore{db: db, sealer: sealer}
}

func (s *StateStore) Put(ctx context.Context, p PendingState) error {
	sealedVerifier, err := s.sealer.Seal([]byte(p.Verifier))
	if err != nil {
		return err
	}
	sealedSecret, err := s.sealer.Seal([]byte(p.ClientSecret))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row := models.OAuthState{
		State:        p.State,
		UserID:       p.UserID,
		AppID:        p.AppID,
		Provider:     p.Provider,
		ServerID:     p.ServerID,
		Verifier:     []byte(sealedVerifier),
		Nonce:        p.Nonce,
		RedirectURI:  p.RedirectURI,
		ClientID:     p.ClientID,
		ClientSecret: []byte(sealedSecret),
		ExpiresAt:    now.Add(stateTTL),
		CreatedAt:    now,
	}
	return s.db.WithContext(ctx).Create(&row).Error
}

// TakeValid loads, deletes (single-use), and validates the state row. It returns
// (nil, nil) for an unknown or expired state. Expired rows are purged on access.
func (s *StateStore) TakeValid(ctx context.Context, state string) (*PendingState, error) {
	now := time.Now().UTC()
	s.db.WithContext(ctx).Where("expires_at < ?", now).Delete(&models.OAuthState{})

	var row models.OAuthState
	err := s.db.WithContext(ctx).Where("state = ?", state).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The delete is the atomic single-use claim: concurrent callers may all read
	// the row, but only the one whose delete actually removes it (RowsAffected==1)
	// proceeds; the losers see 0 rows and back off.
	del := s.db.WithContext(ctx).Where("state = ?", state).Delete(&models.OAuthState{})
	if del.Error != nil {
		return nil, del.Error
	}
	if del.RowsAffected == 0 {
		return nil, nil
	}
	if row.ExpiresAt.Before(now) {
		return nil, nil
	}
	verifier, err := s.sealer.Open(string(row.Verifier))
	if err != nil {
		return nil, err
	}
	var clientSecret []byte
	if len(row.ClientSecret) > 0 {
		if clientSecret, err = s.sealer.Open(string(row.ClientSecret)); err != nil {
			return nil, err
		}
	}
	return &PendingState{
		State:        row.State,
		UserID:       row.UserID,
		AppID:        row.AppID,
		Provider:     row.Provider,
		ServerID:     row.ServerID,
		Verifier:     string(verifier),
		Nonce:        row.Nonce,
		RedirectURI:  row.RedirectURI,
		ClientID:     row.ClientID,
		ClientSecret: string(clientSecret),
	}, nil
}
