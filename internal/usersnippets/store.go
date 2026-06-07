// Package usersnippets is the per-user, per-app saved-prompt store. A snippet is
// a title + body (with an optional emoji + tags) the chat composer inserts — it
// is never consumed by the agent/runtime, purely a client convenience. It lives
// in the daemon's metadata DB (not the app bundle) so it survives app upgrades.
package usersnippets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// ErrNotFound : no snippet matches the (id, user, app) triple.
var ErrNotFound = errors.New("usersnippets: snippet not found")

// ErrInvalidInput : a required field (title/body) is missing or empty.
var ErrInvalidInput = errors.New("usersnippets: invalid input")

// Snippet is the store's view of a user snippet.
type Snippet struct {
	ID        string    `json:"id"`
	AppID     string    `json:"app_id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Emoji     string    `json:"emoji,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the GORM-backed CRUD over user snippets.
type Store struct{ db *gorm.DB }

// NewStore binds a Store to the daemon's metadata DB.
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func toSnippet(r models.UserSnippet) Snippet {
	return Snippet{
		ID:        r.ID,
		AppID:     r.AppID,
		Title:     r.Title,
		Body:      r.Body,
		Emoji:     r.Emoji,
		Tags:      r.Tags,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// List returns the user's snippets for an app, most-recently-updated first.
func (s *Store) List(ctx context.Context, userID, appID string) ([]Snippet, error) {
	var rows []models.UserSnippet
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND app_id = ?", userID, appID).
		Order("updated_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Snippet, len(rows))
	for i := range rows {
		out[i] = toSnippet(rows[i])
	}
	return out, nil
}

// Create inserts a new snippet. Title and body are required.
func (s *Store) Create(ctx context.Context, userID, appID, title, body, emoji string, tags []string) (Snippet, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Snippet{}, fmt.Errorf("%w: title required", ErrInvalidInput)
	}
	if strings.TrimSpace(body) == "" {
		return Snippet{}, fmt.Errorf("%w: body required", ErrInvalidInput)
	}
	now := time.Now().UTC()
	row := models.UserSnippet{
		ID:        uuid.NewString(),
		UserID:    userID,
		AppID:     appID,
		Title:     title,
		Body:      body,
		Emoji:     strings.TrimSpace(emoji),
		Tags:      tags,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Snippet{}, err
	}
	return toSnippet(row), nil
}

// Update applies a partial change. nil pointers leave a field untouched.
// Returns ErrNotFound when the snippet isn't the caller's.
func (s *Store) Update(ctx context.Context, userID, appID, id string, title, body, emoji *string, tags *[]string) (Snippet, error) {
	var row models.UserSnippet
	err := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND app_id = ?", id, userID, appID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Snippet{}, ErrNotFound
	}
	if err != nil {
		return Snippet{}, err
	}

	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" {
			return Snippet{}, fmt.Errorf("%w: title cannot be empty", ErrInvalidInput)
		}
		row.Title = t
	}
	if body != nil {
		if strings.TrimSpace(*body) == "" {
			return Snippet{}, fmt.Errorf("%w: body cannot be empty", ErrInvalidInput)
		}
		row.Body = *body
	}
	if emoji != nil {
		row.Emoji = strings.TrimSpace(*emoji)
	}
	if tags != nil {
		row.Tags = *tags
	}
	row.UpdatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return Snippet{}, err
	}
	return toSnippet(row), nil
}

// Delete hard-deletes the snippet. Returns ErrNotFound when nothing matched the
// (id, user, app) triple — never reports another user's row.
func (s *Store) Delete(ctx context.Context, userID, appID, id string) error {
	res := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND app_id = ?", id, userID, appID).
		Delete(&models.UserSnippet{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
