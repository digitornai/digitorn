// Package userauth lets the background service act on behalf of a user the same
// way any other daemon client does: it holds that user's refresh token, keeps a
// fresh access token by exchanging it against the auth service, and hands the
// valid access token to the daemonclient as the Bearer. This is what lets a
// background-triggered turn (a Discord message, a schedule) authorize against
// the LLM gateway, which requires a real per-user JWT — without depending on a
// session's last-seen token.
package userauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserToken is one user's stored credential for the background service. The
// refresh token is the long-lived secret; the access token + expiry are a cache
// of the last refresh so a restart doesn't force an immediate round-trip.
type UserToken struct {
	UserID       string `gorm:"size:128;primaryKey"`
	RefreshToken string `gorm:"type:text"`
	AccessToken  string `gorm:"type:text"`
	ExpiresAt    time.Time
	UpdatedAt    time.Time
}

func (UserToken) TableName() string { return "bg_user_token" }

// Store persists per-user tokens in the background service's own DB.
type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Migrate() error { return s.db.AutoMigrate(&UserToken{}) }

func (s *Store) Get(ctx context.Context, userID string) (UserToken, bool) {
	var row UserToken
	if err := s.db.WithContext(ctx).First(&row, "user_id = ?", userID).Error; err != nil {
		return UserToken{}, false
	}
	return row, true
}

// Upsert stores/updates a user's tokens. A blank refresh keeps the existing one
// (so a pure access-token refresh never drops the long-lived secret).
func (s *Store) Upsert(ctx context.Context, t UserToken) error {
	t.UpdatedAt = time.Now().UTC()
	cols := []string{"updated_at"}
	if t.AccessToken != "" {
		cols = append(cols, "access_token", "expires_at")
	}
	if t.RefreshToken != "" {
		cols = append(cols, "refresh_token")
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns(cols),
	}).Create(&t).Error
}

// TokenResult is the auth service's /auth/refresh response (the subset we need).
type TokenResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	UserID       string `json:"user_id"`
}

// Client exchanges a refresh token against the auth service (POST /auth/refresh,
// body {refresh_token} — no client credentials required).
type Client struct {
	base string
	hc   *http.Client
}

func NewClient(authURL string) *Client {
	return &Client{
		base: strings.TrimRight(authURL, "/"),
		hc:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenResult, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return TokenResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return TokenResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TokenResult{}, fmt.Errorf("auth refresh: status %d", resp.StatusCode)
	}
	var out TokenResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return TokenResult{}, fmt.Errorf("auth refresh: decode: %w", err)
	}
	if out.AccessToken == "" {
		return TokenResult{}, fmt.Errorf("auth refresh: empty access token")
	}
	return out, nil
}

// refreshMargin is how long before expiry we proactively refresh, so a turn
// never starts with an about-to-expire token.
const refreshMargin = 90 * time.Second

// Manager returns a valid access token for a user, refreshing transparently. It
// is non-blocking on the hot path: an unexpired cached token returns instantly;
// only a cold/expired token triggers the single-flighted refresh round-trip.
type Manager struct {
	store  *Store
	client *Client

	mu  sync.Mutex
	mem map[string]UserToken
}

func NewManager(store *Store, client *Client) *Manager {
	return &Manager{store: store, client: client, mem: map[string]UserToken{}}
}

// Save records a user's refresh token (the handoff from the daemon) and clears
// any stale cached access token so the next Token() refreshes immediately.
func (m *Manager) Save(ctx context.Context, userID, refreshToken string) error {
	if userID == "" || refreshToken == "" {
		return nil
	}
	if err := m.store.Upsert(ctx, UserToken{UserID: userID, RefreshToken: refreshToken}); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.mem, userID)
	m.mu.Unlock()
	return nil
}

// Token returns a currently-valid access token for userID, refreshing via the
// auth service when the cached one is missing or within refreshMargin of expiry.
// Returns "" (no error) when the user has no stored refresh token — the caller
// then falls back to its existing auth (service JWT / dev pinning).
func (m *Manager) Token(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	m.mu.Lock()
	cached, ok := m.mem[userID]
	m.mu.Unlock()
	if !ok {
		row, found := m.store.Get(ctx, userID)
		if !found {
			return "", nil
		}
		cached = row
	}
	if cached.AccessToken != "" && time.Until(cached.ExpiresAt) > refreshMargin {
		return cached.AccessToken, nil
	}
	if cached.RefreshToken == "" {
		return "", nil
	}

	res, err := m.client.Refresh(ctx, cached.RefreshToken)
	if err != nil {
		// Fall back to a still-valid cached access token if the refresh is down.
		if cached.AccessToken != "" && time.Now().Before(cached.ExpiresAt) {
			return cached.AccessToken, nil
		}
		return "", err
	}
	next := UserToken{
		UserID:       userID,
		RefreshToken: res.RefreshToken, // rolled by the auth service
		AccessToken:  res.AccessToken,
		ExpiresAt:    time.Now().Add(time.Duration(res.ExpiresIn) * time.Second),
	}
	_ = m.store.Upsert(ctx, next)
	if next.RefreshToken == "" {
		next.RefreshToken = cached.RefreshToken
	}
	m.mu.Lock()
	m.mem[userID] = next
	m.mu.Unlock()
	return next.AccessToken, nil
}
