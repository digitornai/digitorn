// Package userskills is the per-user, per-app skill store. A user skill is a
// named slash-command directive (instructions the agent loads via use_skill),
// owned by a user and scoped to one app. It lives in the daemon's metadata DB
// — NOT the app bundle — so it survives app upgrades, and is merged with the
// app's own bundled skills into a single skill registry at runtime.
package userskills

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

var (
	// ErrNotFound : no skill matches the (id, user, app) triple.
	ErrNotFound = errors.New("userskills: skill not found")
	// ErrNameConflict : the user already has a skill with this name in this app.
	ErrNameConflict = errors.New("userskills: a skill with this name already exists")
	// ErrInvalidName : the name is not a valid slug.
	ErrInvalidName = errors.New("userskills: invalid skill name")
)

// nameRE is the slug rule, identical across create/update : 1..64 chars,
// lowercase letters/digits/hyphens, starting with a letter or digit.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// NormalizeName trims + lower-cases the name, then validates the slug.
func NormalizeName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if !nameRE.MatchString(n) {
		return "", ErrInvalidName
	}
	return n, nil
}

// Skill is the store's view of a user skill.
type Skill struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	AppID        string    `json:"app_id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Instructions string    `json:"instructions"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store is the GORM-backed CRUD over user skills.
type Store struct{ db *gorm.DB }

// NewStore binds a Store to the daemon's metadata DB.
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func toSkill(r models.UserSkill) Skill {
	return Skill{
		ID:           r.ID,
		UserID:       r.UserID,
		AppID:        r.AppID,
		Name:         r.Name,
		Description:  r.Description,
		Instructions: r.Instructions,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

// List returns the user's skills for an app, most-recently-updated first.
func (s *Store) List(ctx context.Context, userID, appID string) ([]Skill, error) {
	var rows []models.UserSkill
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND app_id = ?", userID, appID).
		Order("updated_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Skill, len(rows))
	for i := range rows {
		out[i] = toSkill(rows[i])
	}
	return out, nil
}

// GetByName resolves a skill by its name within (user, app). The found flag is
// false (with a nil error) when no skill matches — the loader uses this to fall
// through to the app's bundled skills.
func (s *Store) GetByName(ctx context.Context, userID, appID, name string) (Skill, bool, error) {
	// Lookup is lenient (unlike create/update validation) : accept the command
	// with or without the leading "/", case-insensitively.
	n := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "/")))
	var row models.UserSkill
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND app_id = ? AND name = ?", userID, appID, n).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Skill{}, false, nil
	}
	if err != nil {
		return Skill{}, false, err
	}
	return toSkill(row), true, nil
}

// Create inserts a new skill. Returns ErrInvalidName for a bad slug and
// ErrNameConflict when the user already has that name in this app.
func (s *Store) Create(ctx context.Context, userID, appID, name, description, instructions string) (Skill, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return Skill{}, err
	}
	if strings.TrimSpace(instructions) == "" {
		return Skill{}, errors.New("userskills: instructions required")
	}

	taken, err := s.nameTaken(ctx, userID, appID, n, "")
	if err != nil {
		return Skill{}, err
	}
	if taken {
		return Skill{}, ErrNameConflict
	}

	now := time.Now().UTC()
	row := models.UserSkill{
		ID:           uuid.NewString(),
		UserID:       userID,
		AppID:        appID,
		Name:         n,
		Description:  strings.TrimSpace(description),
		Instructions: instructions,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Skill{}, err
	}
	return toSkill(row), nil
}

// Update applies a partial change. nil pointers leave a field untouched. A name
// change is re-validated and conflict-checked (excluding the row itself).
// Returns ErrNotFound when the skill isn't the caller's.
func (s *Store) Update(ctx context.Context, userID, appID, id string, name, description, instructions *string) (Skill, error) {
	var row models.UserSkill
	err := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND app_id = ?", id, userID, appID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, err
	}

	if name != nil {
		n, err := NormalizeName(*name)
		if err != nil {
			return Skill{}, err
		}
		if n != row.Name {
			taken, err := s.nameTaken(ctx, userID, appID, n, row.ID)
			if err != nil {
				return Skill{}, err
			}
			if taken {
				return Skill{}, ErrNameConflict
			}
			row.Name = n
		}
	}
	if description != nil {
		row.Description = strings.TrimSpace(*description)
	}
	if instructions != nil {
		if strings.TrimSpace(*instructions) == "" {
			return Skill{}, errors.New("userskills: instructions cannot be empty")
		}
		row.Instructions = *instructions
	}
	row.UpdatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return Skill{}, err
	}
	return toSkill(row), nil
}

// Delete hard-deletes the skill. Returns ErrNotFound when nothing matched the
// (id, user, app) triple — never reports another user's row.
func (s *Store) Delete(ctx context.Context, userID, appID, id string) error {
	res := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND app_id = ?", id, userID, appID).
		Delete(&models.UserSkill{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// nameTaken reports whether (user, app, name) is used by a row other than
// excludeID (pass "" to consider all rows).
func (s *Store) nameTaken(ctx context.Context, userID, appID, name, excludeID string) (bool, error) {
	q := s.db.WithContext(ctx).Model(&models.UserSkill{}).
		Where("user_id = ? AND app_id = ? AND name = ?", userID, appID, name)
	if excludeID != "" {
		q = q.Where("id <> ?", excludeID)
	}
	var count int64
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
