package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/digitornai/digitorn/internal/persistence/models"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// modelDefault is one agent's user-chosen default model for NEW sessions of an
// app. Limits mirror the per-session override fields (0 = unknown/none).
type modelDefault struct {
	Model            string `json:"model"`
	MaxContextTokens int    `json:"max_ctx_tokens,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
}

// modelDefaultsStore persists per-user per-app per-agent default models.
// Plain values (not secret) → gorm only, with a small read-through cache so
// session creation never adds a DB read on the hot path after first use.
type modelDefaultsStore struct {
	mu    sync.RWMutex
	cache map[string]map[string]modelDefault // userID|appID -> agentID -> default
	db    *gorm.DB
}

func newModelDefaultsStore(db *gorm.DB) *modelDefaultsStore {
	return &modelDefaultsStore{cache: map[string]map[string]modelDefault{}, db: db}
}

func (s *modelDefaultsStore) key(userID, appID string) string { return userID + "|" + appID }

func (s *modelDefaultsStore) get(userID, appID string) map[string]modelDefault {
	if s == nil {
		return nil
	}
	k := s.key(userID, appID)
	s.mu.RLock()
	if m, ok := s.cache[k]; ok {
		s.mu.RUnlock()
		return m
	}
	s.mu.RUnlock()

	out := map[string]modelDefault{}
	if s.db != nil {
		var rows []models.UserAppModelDefault
		if err := s.db.Where("user_id = ? AND app_id = ?", userID, appID).Find(&rows).Error; err == nil {
			for _, r := range rows {
				out[r.AgentID] = modelDefault{Model: r.Model, MaxContextTokens: r.MaxContextTokens, MaxOutputTokens: r.MaxOutputTokens}
			}
		}
	}
	s.mu.Lock()
	s.cache[k] = out
	s.mu.Unlock()
	return out
}

// set replaces the full map for (user, app): provided agents are upserted,
// agents absent from `defs` (or with an empty model) are deleted.
func (s *modelDefaultsStore) set(userID, appID string, defs map[string]modelDefault) error {
	if s == nil {
		return nil
	}
	clean := map[string]modelDefault{}
	for agentID, d := range defs {
		agentID = strings.TrimSpace(agentID)
		d.Model = strings.TrimSpace(d.Model)
		if agentID == "" || d.Model == "" {
			continue
		}
		clean[agentID] = d
	}
	if s.db != nil {
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("user_id = ? AND app_id = ?", userID, appID).
				Delete(&models.UserAppModelDefault{}).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			for agentID, d := range clean {
				row := models.UserAppModelDefault{
					UserID: userID, AppID: appID, AgentID: agentID,
					Model: d.Model, MaxContextTokens: d.MaxContextTokens, MaxOutputTokens: d.MaxOutputTokens,
					UpdatedAt: now,
				}
				if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.cache[s.key(userID, appID)] = clean
	s.mu.Unlock()
	return nil
}

// getModelDefaults handles GET /api/apps/{app_id}/model-defaults.
func (d *Daemon) getModelDefaults(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	defs := d.modelDefaults.get(userIDOf(r.Context()), appID)
	writeJSON(w, http.StatusOK, map[string]any{"defaults": defs, "count": len(defs)})
}

// putModelDefaults handles PUT /api/apps/{app_id}/model-defaults with body
// {"defaults": {agentID: {model, max_ctx_tokens?, max_output_tokens?}}} —
// full-replace semantics (an agent omitted or with model:"" reverts to the
// app YAML's brain default for future sessions).
func (d *Daemon) putModelDefaults(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	var req struct {
		Defaults map[string]modelDefault `json:"defaults"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	uid := userIDOf(r.Context())
	if err := d.modelDefaults.set(uid, appID, req.Defaults); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	defs := d.modelDefaults.get(uid, appID)
	writeJSON(w, http.StatusOK, map[string]any{"defaults": defs, "count": len(defs)})
}

// directVaultProvider returns the provider slug when `model` is one of the
// user's vault models the runtime can route directly (cross-provider pin);
// "" otherwise (gateway/declared models need no pin).
func (d *Daemon) directVaultProvider(ctx context.Context, userID, model string) string {
	if d.creds == nil || model == "" {
		return ""
	}
	for _, m := range d.creds.ListUserModels(ctx, userID) {
		if m.ID == model && m.Direct {
			return m.OwnedBy
		}
	}
	return ""
}

// applyModelDefaults pins the user's per-app default models on a NEW session,
// BEFORE its first turn. skipAgent (the entry agent when the launcher already
// pinned an explicit model) wins over the stored default.
func (d *Daemon) applyModelDefaults(ctx context.Context, userID, appID, sid, skipAgent string) {
	defs := d.modelDefaults.get(userID, appID)
	if len(defs) == 0 {
		return
	}
	for agentID, def := range defs {
		if agentID == skipAgent || def.Model == "" {
			continue
		}
		meta := &sessionstore.MetaPayload{
			Model:            def.Model,
			AgentID:          agentID,
			Provider:         d.directVaultProvider(ctx, userID, def.Model),
			MaxContextTokens: def.MaxContextTokens,
			MaxOutputTokens:  def.MaxOutputTokens,
		}
		if meta.MaxContextTokens <= 0 {
			meta.MaxContextTokens = d.gatewayModelWindow(def.Model)
		}
		if _, err := d.sessionStore.AppendDurable(ctx, sessionstore.Event{
			Type:      sessionstore.EventModelChanged,
			SessionID: sid,
			AppID:     appID,
			UserID:    userID,
			Meta:      meta,
		}); err != nil {
			d.logger.Warn("createSession: model default failed", "sid", sid, "agent", agentID, "err", err.Error())
		}
	}
}
