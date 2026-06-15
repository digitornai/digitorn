package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ── Automations : the USER-SCOPED window onto the background service ─────────
//
// The background service's /ops API is an ADMIN surface (static bearer, no
// per-user scoping) — it must never reach a browser. These routes are the
// client-facing twin : the daemon authenticates the caller (JWT), enforces
// OWNERSHIP on every read and write, and relays server-side with the ops
// token. A user can only ever see, create, enable or disable THEIR schedules,
// and only read runs born from THEIR triggers — the cross-user isolation the
// rest of the daemon already guarantees, extended to automations.

// opsSchedule is the slice of the bg /ops/schedules row the daemon consumes.
type opsSchedule struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	SessionID string `json:"session_id"`
	Owner     string `json:"owner"`
	Schedule  string `json:"schedule"`
	Message   string `json:"message"`
	Enabled   bool   `json:"enabled"`
	NextRun   string `json:"next_run,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// opsRun is the slice of the bg /ops/runs row the daemon consumes.
type opsRun struct {
	ID           string `json:"id"`
	TriggerID    string `json:"trigger_id"`
	AppID        string `json:"app_id"`
	Provider     string `json:"provider"`
	Outcome      string `json:"outcome"`
	SessionID    string `json:"session_id,omitempty"`
	ReplyChars   int    `json:"reply_chars,omitempty"`
	ReplyPreview string `json:"reply_preview,omitempty"`
	Error        string `json:"error,omitempty"`
	StartedAt    string `json:"started_at"`
	EndedAt      string `json:"ended_at"`
	DurationMs   int64  `json:"duration_ms"`
}

// ownSchedules filters the bg schedule list down to one user's rows. Pure —
// the ownership chokepoint for every read path.
func ownSchedules(all []opsSchedule, user string) []opsSchedule {
	out := make([]opsSchedule, 0, len(all))
	for _, s := range all {
		if s.Owner != "" && s.Owner == user {
			out = append(out, s)
		}
	}
	return out
}

// ownRuns keeps only the runs born from the given trigger ids (the user's
// schedules). Pure — the ownership chokepoint for the runs read path.
func ownRuns(all []opsRun, triggers map[string]bool) []opsRun {
	out := make([]opsRun, 0, len(all))
	for _, r := range all {
		if triggers[r.TriggerID] {
			out = append(out, r)
		}
	}
	return out
}

// opsClient is the daemon's server-side client for the bg ops API.
type opsClient struct {
	base  string
	token string
	http  *http.Client
}

func (d *Daemon) opsClient() (*opsClient, error) {
	base := strings.TrimRight(d.cfg.Background.OpsURL, "/")
	if base == "" {
		return nil, errors.New("automations are not configured on this daemon")
	}
	return &opsClient{base: base, token: d.cfg.Background.OpsToken, http: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (c *opsClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("background service unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("background service: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *opsClient) schedules(ctx context.Context) ([]opsSchedule, error) {
	var env struct {
		Schedules []opsSchedule `json:"schedules"`
	}
	if err := c.do(ctx, http.MethodGet, "/ops/schedules", nil, &env); err != nil {
		return nil, err
	}
	return env.Schedules, nil
}

// userSchedule loads ONE schedule and enforces ownership : a schedule that is
// missing OR belongs to someone else is the same 404 (existence is not leaked).
func (c *opsClient) userSchedule(ctx context.Context, id, user string) (opsSchedule, error) {
	all, err := c.schedules(ctx)
	if err != nil {
		return opsSchedule{}, err
	}
	for _, s := range ownSchedules(all, user) {
		if s.ID == id {
			return s, nil
		}
	}
	return opsSchedule{}, errSessionNotFound // generic not-found, no ownership leak
}

// GET /api/automations/schedules — the caller's schedules only.
func (d *Daemon) listAutomationSchedules(w http.ResponseWriter, r *http.Request) {
	c, err := d.opsClient()
	if err != nil {
		writeError(w, http.StatusNotFound, "not_configured", err.Error())
		return
	}
	all, err := c.schedules(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "background_unavailable", err.Error())
		return
	}
	mine := ownSchedules(all, userIDOf(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"schedules": mine, "count": len(mine)})
}

// automationCreateRequest is what a client may set. Owner is NOT accepted from
// the body — it is always the authenticated caller.
type automationCreateRequest struct {
	AppID       string `json:"app_id"`
	SessionID   string `json:"session_id"`
	Schedule    string `json:"schedule"`
	Message     string `json:"message"`
	Context     string `json:"context"`
	Reply       string `json:"reply"`
	Reports     bool   `json:"reports"`
	Attachments []struct {
		Hash string `json:"hash"`
		Mime string `json:"mime"`
		Size int64  `json:"size"`
	} `json:"attachments"`
}

// POST /api/automations/schedules — create a schedule owned by the caller.
func (d *Daemon) createAutomationSchedule(w http.ResponseWriter, r *http.Request) {
	c, err := d.opsClient()
	if err != nil {
		writeError(w, http.StatusNotFound, "not_configured", err.Error())
		return
	}
	var req automationCreateRequest
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.AppID == "" || req.SessionID == "" || req.Schedule == "" || req.Message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "app_id, session_id, schedule and message are required")
		return
	}
	body := map[string]any{
		"app_id":     req.AppID,
		"session_id": req.SessionID,
		"owner":      userIDOf(r.Context()), // ALWAYS the caller — never client-supplied
		"schedule":   req.Schedule,
		"message":    req.Message,
		"context":    req.Context,
		"reply":      req.Reply,
		"reports":    req.Reports,
	}
	if len(req.Attachments) > 0 {
		body["attachments"] = req.Attachments
	}
	var created map[string]any
	if err := c.do(r.Context(), http.MethodPost, "/ops/schedules", body, &created); err != nil {
		writeError(w, http.StatusBadGateway, "background_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// POST /api/automations/schedules/{id}/enable|disable — ownership-checked toggle.
func (d *Daemon) toggleAutomationSchedule(enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := d.opsClient()
		if err != nil {
			writeError(w, http.StatusNotFound, "not_configured", err.Error())
			return
		}
		id := chi.URLParam(r, "id")
		if _, err := c.userSchedule(r.Context(), id, userIDOf(r.Context())); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "schedule not found")
			return
		}
		action := "disable"
		if enable {
			action = "enable"
		}
		if err := c.do(r.Context(), http.MethodPost, "/ops/triggers/"+id+"/"+action, nil, nil); err != nil {
			writeError(w, http.StatusBadGateway, "background_unavailable", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": enable})
	}
}

// GET /api/automations/runs[?app=] — runs born from the caller's triggers only.
func (d *Daemon) listAutomationRuns(w http.ResponseWriter, r *http.Request) {
	c, err := d.opsClient()
	if err != nil {
		writeError(w, http.StatusNotFound, "not_configured", err.Error())
		return
	}
	user := userIDOf(r.Context())
	all, err := c.schedules(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "background_unavailable", err.Error())
		return
	}
	mine := map[string]bool{}
	for _, s := range ownSchedules(all, user) {
		mine[s.ID] = true
	}
	q := "/ops/runs?limit=" + fmt.Sprint(parseIntQuery(r, "limit", 50))
	if app := r.URL.Query().Get("app"); app != "" {
		q += "&app_id=" + app
	}
	var env struct {
		Runs []opsRun `json:"runs"`
	}
	if err := c.do(r.Context(), http.MethodGet, q, nil, &env); err != nil {
		writeError(w, http.StatusBadGateway, "background_unavailable", err.Error())
		return
	}
	runs := ownRuns(env.Runs, mine)
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "count": len(runs)})
}
