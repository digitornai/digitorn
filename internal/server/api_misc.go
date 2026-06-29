package server

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
	"github.com/mbathepaul/digitorn/internal/runtime/policy/approval"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// ---------- Approvals ----------

func (d *Daemon) listApprovals(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	summaries, err := d.walkSessionMeta(appID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	type pending struct {
		SessionID  string         `json:"session_id"`
		ApprovalID string         `json:"approval_id"`
		Kind       string         `json:"kind"`
		CreatedAt  string         `json:"created_at"`
		Payload    map[string]any `json:"payload,omitempty"`
	}
	out := []pending{}
	for _, s := range summaries {
		st, err := d.sessionStore.State(s.SessionID)
		if err != nil {
			continue
		}
		st.RLock()
		for id, ap := range st.Approvals {
			if ap.Status != "" && ap.Status != "pending" {
				continue
			}
			out = append(out, pending{
				SessionID:  s.SessionID,
				ApprovalID: id,
				Kind:       ap.Kind,
				CreatedAt:  isoNano(ap.CreatedAt),
				Payload:    ap.Payload,
			})
		}
		st.RUnlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out, "count": len(out)})
}

type resolveApprovalRequest struct {
	SessionID  string `json:"session_id"`
	ApprovalID string `json:"approval_id"`
	Action     string `json:"action"`
	Reason     string `json:"reason,omitempty"`
}

func (d *Daemon) resolveApproval(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	var req resolveApprovalRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.SessionID == "" || req.ApprovalID == "" || req.Action == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session_id, approval_id, action required")
		return
	}
	if _, err := d.requireOwnedSession(r.Context(), req.SessionID); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}

	typ := sessionstore.EventApprovalGranted
	switch req.Action {
	case "deny", "denied", "reject":
		typ = sessionstore.EventApprovalDenied
	}
	ev := sessionstore.Event{
		Type:      typ,
		SessionID: req.SessionID,
		AppID:     appID,
		UserID:    userIDOf(r.Context()),
		Approval: &sessionstore.ApprovalPayload{
			ID:     req.ApprovalID,
			Status: req.Action,
			Reason: req.Reason,
		},
	}
	seq, err := d.sessionStore.Append(r.Context(), ev)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "append_failed", err.Error())
		return
	}

	// SG-5 : signal the approval registry so any goroutine blocked
	// in awaitApproval() unblocks with the resolved Result. The
	// registry is process-local ; if the daemon was restarted
	// between the EventApprovalRequest and this resolve call, no
	// waiter exists and Resolve is a no-op (the durable event has
	// already landed but the original turn is gone).
	//
	// resolved tells the CALLER whether a live waiter was actually signaled. A
	// false here is exactly the "I clicked y and nothing happened" symptom : the
	// approval already timed out / was superseded by a retry / the turn is gone,
	// so the durable grant lands but no turn resumes. Surfacing it lets the client
	// say "this approval expired" instead of going silent.
	resolved := false
	if d.approvalRegistry != nil {
		result := approval.ResultApproved
		switch req.Action {
		case "denied", "deny", "reject":
			result = approval.ResultDenied
		case "approved_always":
			result = approval.ResultApprovedAlways
		}
		resolved = d.approvalRegistry.Resolve(req.ApprovalID, approval.Resolution{
			Result: result,
			Reason: req.Reason,
		})
	}
	if !resolved {
		d.logger.Warn("approval resolve found no live waiter (stale/expired/superseded)",
			slog.String("approval_id", req.ApprovalID),
			slog.String("session_id", req.SessionID),
			slog.String("action", req.Action))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":  req.SessionID,
		"approval_id": req.ApprovalID,
		"status":      req.Action,
		"resolved":    resolved,
		"seq":         seq,
	})
}

// ---------- Secrets (in-memory V1) ----------

// secretSealer encrypts a secret value at rest. *mcpoauth.Sealer satisfies it.
type secretSealer interface {
	Seal([]byte) (string, error)
	Open(string) ([]byte, error)
}

// secretStore holds per-user per-app secrets. When db+sealer are set it persists
// them encrypted (survives restart, readable by the background service through
// the daemon); the in-memory map is a write-through cache. With no sealer it
// degrades to memory-only (dev / no key file).
type secretStore struct {
	mu     sync.RWMutex
	data   map[string]map[string]map[string]string
	db     *gorm.DB
	sealer secretSealer
}

func newSecretStore() *secretStore {
	return &secretStore{data: map[string]map[string]map[string]string{}}
}

func newPersistedSecretStore(db *gorm.DB, sealer secretSealer) *secretStore {
	return &secretStore{
		data:   map[string]map[string]map[string]string{},
		db:     db,
		sealer: sealer,
	}
}

func (s *secretStore) persists() bool { return s.db != nil && s.sealer != nil }

func (s *secretStore) get(user, app, key string) (string, bool) {
	s.mu.RLock()
	if u, ok := s.data[user]; ok {
		if a, ok := u[app]; ok {
			if v, ok := a[key]; ok {
				s.mu.RUnlock()
				return v, true
			}
		}
	}
	s.mu.RUnlock()
	if !s.persists() {
		return "", false
	}
	var row models.UserAppSecret
	if err := s.db.Where("user_id = ? AND app_id = ? AND key = ?", user, app, key).
		First(&row).Error; err != nil {
		return "", false
	}
	plain, err := s.sealer.Open(row.Sealed)
	if err != nil {
		return "", false
	}
	v := string(plain)
	s.cacheSet(user, app, key, v)
	return v, true
}

// getAny returns a secret for (app, key) regardless of which user set it —
// installation-scoped resolution for shared resources like a channel bot token
// (one bot per app install). Used when the daemon resolves `{{secret.X}}` in a
// channel's config before pushing it to the background service.
func (s *secretStore) getAny(app, key string) (string, bool) {
	if s.persists() {
		var row models.UserAppSecret
		if err := s.db.Where("app_id = ? AND key = ?", app, key).First(&row).Error; err == nil {
			if plain, err := s.sealer.Open(row.Sealed); err == nil {
				return string(plain), true
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, apps := range s.data {
		if a, ok := apps[app]; ok {
			if v, ok := a[key]; ok {
				return v, true
			}
		}
	}
	return "", false
}

func (s *secretStore) all(user, app string) map[string]string {
	out := map[string]string{}
	if s.persists() {
		var rows []models.UserAppSecret
		if err := s.db.Where("user_id = ? AND app_id = ?", user, app).Find(&rows).Error; err == nil {
			for _, r := range rows {
				out[r.Key] = "********"
			}
			return out
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u, ok := s.data[user]; ok {
		if a, ok := u[app]; ok {
			for k := range a {
				out[k] = "********"
			}
		}
	}
	return out
}

func (s *secretStore) set(user, app, key, value string) {
	s.cacheSet(user, app, key, value)
	if !s.persists() {
		return
	}
	sealed, err := s.sealer.Seal([]byte(value))
	if err != nil {
		return
	}
	row := models.UserAppSecret{UserID: user, AppID: app, Key: key, Sealed: sealed, UpdatedAt: time.Now().UTC()}
	s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "app_id"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"sealed", "updated_at"}),
	}).Create(&row)
}

func (s *secretStore) cacheSet(user, app, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[user]; !ok {
		s.data[user] = map[string]map[string]string{}
	}
	if _, ok := s.data[user][app]; !ok {
		s.data[user][app] = map[string]string{}
	}
	s.data[user][app][key] = value
}

func (s *secretStore) delete(user, app, key string) bool {
	s.mu.Lock()
	if u, ok := s.data[user]; ok {
		if a, ok := u[app]; ok {
			delete(a, key)
		}
	}
	s.mu.Unlock()
	if !s.persists() {
		return true
	}
	res := s.db.Where("user_id = ? AND app_id = ? AND key = ?", user, app, key).
		Delete(&models.UserAppSecret{})
	return res.Error == nil && res.RowsAffected > 0
}

func (d *Daemon) ensureSecretStore() *secretStore {
	d.secretsOnce.Do(func() {
		if d.secrets == nil {
			d.secrets = newSecretStore()
		}
	})
	return d.secrets
}

func (d *Daemon) requiredSecrets(w http.ResponseWriter, r *http.Request) {
	// V1 stub : the runtime/compiler will eventually feed this from the
	// app manifest's `security.credentials_schema`. For now : empty.
	writeJSON(w, http.StatusOK, map[string]any{"required": []any{}, "providers": []any{}})
}

func (d *Daemon) listSecrets(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())
	all := d.ensureSecretStore().all(uid, appID)
	writeJSON(w, http.StatusOK, map[string]any{"secrets": all, "count": len(all)})
}

func (d *Daemon) getSecret(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	key := chi.URLParam(r, "key")
	uid := userIDOf(r.Context())
	v, ok := d.ensureSecretStore().get(uid, appID, key)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "secret not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": v})
}

type setSecretsRequest map[string]string

func (d *Daemon) setSecrets(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())
	var req setSecretsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s := d.ensureSecretStore()
	for k, v := range req {
		if v == secretSentinel {
			continue // a redacted echo: keep the stored value, never overwrite
		}
		s.set(uid, appID, k, v)
	}
	d.repushChannelTriggers(appID, uid)
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(req)})
}

// bgAuthApp / bgAuthKey namespace the caller's auth refresh token in the secret
// store (encrypted at rest). It is per-user, not per-app.
const bgAuthApp = "__bg_auth__"
const bgAuthKey = "refresh_token"

// repushChannelTriggers re-resolves an app's channel configs (with freshly saved
// secrets) and pushes them to the background as `userID`, attaching that user's
// stored refresh token so background turns get a real per-user JWT. Best-effort
// + async: never blocks the response.
func (d *Daemon) repushChannelTriggers(appID, userID string) {
	if d.cfg.Background.OpsURL == "" {
		return
	}
	refresh, _ := d.ensureSecretStore().get(userID, bgAuthApp, bgAuthKey)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if ra, err := d.appMgr.Get(ctx, appID); err == nil && ra != nil && ra.Meta != nil {
			d.pushTriggersAs(ctx, ra.Meta, userID, refresh)
		}
	}()
}

// setBackgroundToken stores the caller's auth refresh token so the background
// service can mint fresh access tokens for their background apps. The web BFF
// (which holds the refresh token) posts it when the user configures a background
// app. Re-pushes nothing here; it lands with the next channel save / install.
func (d *Daemon) setBackgroundToken(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r.Context())
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if body.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "refresh_token required")
		return
	}
	d.ensureSecretStore().set(uid, bgAuthApp, bgAuthKey, body.RefreshToken)
	writeJSON(w, http.StatusOK, map[string]any{"stored": true})
}

type setSecretRequest struct {
	Value string `json:"value"`
}

func (d *Daemon) setSecret(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	key := chi.URLParam(r, "key")
	uid := userIDOf(r.Context())
	var req setSecretRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Value != secretSentinel { // ignore a redacted echo; keep the stored value
		d.ensureSecretStore().set(uid, appID, key, req.Value)
	}
	d.repushChannelTriggers(appID, uid)
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "updated": true})
}

func (d *Daemon) deleteSecret(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	key := chi.URLParam(r, "key")
	uid := userIDOf(r.Context())
	if !d.ensureSecretStore().delete(uid, appID, key) {
		writeError(w, http.StatusNotFound, "not_found", "secret not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "deleted": true})
}

// ---------- Diagnostics / Status ----------

func (d *Daemon) diagnostics(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())
	summaries, _ := d.walkSessionMeta(appID, uid)
	flusherStats := d.sessionFlusher.Stats()
	busStats := d.sessionStore.Stats()
	bridgeStats := d.bridge.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id":      appID,
		"instance_id": d.envelopeBuilder.InstanceID,
		"sessions": map[string]any{
			"total_for_user": len(summaries),
		},
		"flusher": map[string]any{
			"written":    flusherStats.TotalWritten,
			"dropped":    flusherStats.TotalDropped,
			"batches":    flusherStats.TotalBatches,
			"queued":     flusherStats.TotalQueued,
			"fd_cached":  flusherStats.TotalFDCached,
			"num_shards": d.sessionFlusher.NumShards(),
		},
		"bus": map[string]any{
			"append_total":     busStats.AppendTotal,
			"append_errors":    busStats.AppendErrors,
			"dropped":          busStats.Dropped,
			"notify_total":     busStats.NotifyTotal,
			"callback_panics":  busStats.CallbackPanics,
			"subscriber_drops": busStats.SubscriberDrops,
			"subscriber_kicks": busStats.SubscriberKicks,
			"states_loaded":    busStats.StatesLoaded,
			"states_evicted":   busStats.StatesEvicted,
			"subscriptions":    busStats.Subscriptions,
		},
		"bridge": map[string]any{
			"connects":         bridgeStats.Connects,
			"disconnects":      bridgeStats.Disconnects,
			"auth_rejected":    bridgeStats.AuthRejected,
			"emits":            bridgeStats.Emits,
			"emit_errors":      bridgeStats.EmitErrors,
			"drops_no_routing": bridgeStats.DropsNoRouting,
			"actions":          bridgeStats.Actions,
			"actions_rejected": bridgeStats.ActionsRejected,
			"live_clients":     bridgeStats.LiveClients,
		},
		"runtime_go": map[string]any{
			"goroutines": runtime.NumGoroutine(),
			"go_version": runtime.Version(),
			"goos":       runtime.GOOS,
			"goarch":     runtime.GOARCH,
		},
		"now": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (d *Daemon) appStatus(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id": appID,
		"status": "running",
		"checks": map[string]bool{
			"session_store": true,
			"realtime":      true,
		},
	})
}

func (d *Daemon) appErrors(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())
	limit := parseIntQuery(r, "limit", 100)
	summaries, _ := d.walkSessionMeta(appID, uid)
	type errEntry struct {
		SessionID string `json:"session_id"`
		Seq       uint64 `json:"seq"`
		Ts        string `json:"ts"`
		Code      string `json:"code,omitempty"`
		Message   string `json:"message"`
		Fatal     bool   `json:"fatal,omitempty"`
	}
	out := []errEntry{}
	for _, s := range summaries {
		state, err := d.sessionStore.State(s.SessionID)
		if err != nil {
			continue
		}
		state.RLock()
		for _, e := range state.Errors {
			out = append(out, errEntry{
				SessionID: s.SessionID,
				Seq:       e.Seq,
				Ts:        isoNano(e.TsUnixNano),
				Code:      e.Code,
				Message:   e.Message,
				Fatal:     e.Fatal,
			})
			if len(out) >= limit {
				break
			}
		}
		state.RUnlock()
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": out, "count": len(out)})
}

func (d *Daemon) uiConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"theme":        "default",
		"capabilities": d.envelopeBuilder.Capabilities,
		"instance_id":  d.envelopeBuilder.InstanceID,
		"socket_path":  "/socket.io/",
		"socket_ns":    "/events",
	})
}

func (d *Daemon) deployStatus(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	// V1: no deployment subsystem yet. Report a static "deployed" stub.
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id":     appID,
		"deployed":   true,
		"status":     "ok",
		"last_check": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// ---------- Daemon-level stats ----------

func (d *Daemon) daemonStats(w http.ResponseWriter, r *http.Request) {
	flusher := d.sessionFlusher.Stats()
	bus := d.sessionStore.Stats()
	bridge := d.bridge.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id":   d.envelopeBuilder.InstanceID,
		"now":           time.Now().UTC().Format(time.RFC3339Nano),
		"goroutines":    runtime.NumGoroutine(),
		"flusher_stats": flusher,
		"bus_stats":     bus,
		"bridge_stats":  bridge,
	})
}
