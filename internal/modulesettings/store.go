// Package modulesettings persists a user's per-app, per-module config DELTAS
// (BYOK mode) and resolves them on the agent hot path without blocking.
//
// The YAML `config:` block is the immutable per-app default. When an app's BYOK
// flag is on, a user's saved deltas deep-merge over that default for that user.
// Deltas are sealed at rest (they hold secrets like DSNs) and cached decrypted
// in memory: a turn's lookup is an O(1) map read, never a DB read + decrypt per
// LLM/tool call. Saving invalidates the cache.
package modulesettings

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

// Sealer is the subset of mcpoauth.Sealer this package needs.
type Sealer interface {
	Seal(plaintext []byte) (string, error)
	Open(encoded string) ([]byte, error)
}

type Store struct {
	db     *gorm.DB
	sealer Sealer
	mu     sync.RWMutex
	cache  map[string]map[string]any // "user|app|module" -> decrypted deltas
}

func NewStore(db *gorm.DB, sealer Sealer) *Store {
	return &Store{db: db, sealer: sealer, cache: map[string]map[string]any{}}
}

func cacheKey(userID, appID, moduleID string) string {
	return userID + "|" + appID + "|" + moduleID
}

// Deltas returns a user's saved config deltas for a module — O(1) cached.
// First miss does one local DB read + decrypt, then caches (incl. the empty
// verdict). Safe on the hot path: no network, decrypts once per key.
func (s *Store) Deltas(ctx context.Context, userID, appID, moduleID string) map[string]any {
	if s == nil || s.db == nil || userID == "" || appID == "" || moduleID == "" {
		return nil
	}
	k := cacheKey(userID, appID, moduleID)

	s.mu.RLock()
	if d, ok := s.cache[k]; ok {
		s.mu.RUnlock()
		return d
	}
	s.mu.RUnlock()

	d := s.load(ctx, userID, appID, moduleID)
	s.mu.Lock()
	s.cache[k] = d
	s.mu.Unlock()
	return d
}

func (s *Store) load(ctx context.Context, userID, appID, moduleID string) map[string]any {
	var row models.UserModuleConfig
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND app_id = ? AND module_id = ?", userID, appID, moduleID).
		First(&row).Error; err != nil {
		return map[string]any{}
	}
	plain, err := s.sealer.Open(row.Sealed)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if len(plain) > 0 {
		_ = json.Unmarshal(plain, &out)
	}
	return out
}

// Set seals + upserts a user's deltas for a module and invalidates the cache.
func (s *Store) Set(ctx context.Context, userID, appID, moduleID string, deltas map[string]any) error {
	if deltas == nil {
		deltas = map[string]any{}
	}
	blob, err := json.Marshal(deltas)
	if err != nil {
		return err
	}
	sealed, err := s.sealer.Seal(blob)
	if err != nil {
		return err
	}
	row := models.UserModuleConfig{
		ID:        uuid.NewString(),
		UserID:    userID,
		AppID:     appID,
		ModuleID:  moduleID,
		Sealed:    sealed,
		UpdatedAt: time.Now().UTC(),
	}
	err = s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "app_id"}, {Name: "module_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"sealed", "updated_at"}),
	}).Create(&row).Error

	s.mu.Lock()
	delete(s.cache, cacheKey(userID, appID, moduleID))
	s.mu.Unlock()
	return err
}
