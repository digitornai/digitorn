package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/digitornai/digitorn/internal/background/adapter/cron"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/store"
)

// CreateTriggerRequest is the body of POST /ops/triggers.
// The Rearm hook persists and arms the trigger live (no restart needed).
type CreateTriggerRequest struct {
	AppID    string `json:"app_id"`
	Provider string `json:"provider"`
	Adapter  string `json:"adapter"` // "cron", "pieces", "rss", "webhook", …
	Schedule string `json:"schedule"`
	Agent    string `json:"agent"`
	Message  string `json:"message"`
	Session  string `json:"session"`
	Reply    string `json:"reply"`
	Owner    string `json:"owner"`
	Context  string `json:"context"`
	Kind     string `json:"kind"`
	Reports  bool   `json:"reports"`
	// RefreshToken is the owner's auth refresh token, handed off so the service
	// can mint fresh per-user access tokens for this app's background turns
	// (the LLM gateway requires a real user JWT). Stored, never logged.
	RefreshToken string `json:"refresh_token,omitempty"`
	// Config carries adapter-specific config stored in ConfigJSON.
	// For pieces: {piece, trigger, auth, props, interval, trigger_url}.
	Config      map[string]any             `json:"config,omitempty"`
	Activation  *channels.ActivationConfig `json:"activation,omitempty"`
	Attachments []channels.AttachmentRef   `json:"attachments"`
}

// OpsConfig configures the ops/admin API. Token, when set, is required as a
// Bearer credential on every /ops route (read AND control) — for a cloud
// deployment where the service port is reachable. Empty → no auth (the default
// for a localhost-bound local daemon, matching the existing /stats surface).
//
// Rearm, when set, programs a trigger at runtime (POST /ops/triggers): it builds
// the durable trigger AND arms the live adapter. nil → POST /ops/triggers returns
// 501 (the wiring that can reach the adapters wasn't provided).
type OpsConfig struct {
	Token  string
	Rearm  func(context.Context, CreateTriggerRequest) (store.Trigger, error)
	Disarm func(context.Context, store.Trigger) error
}

// opsRoutes builds the ops/admin HTTP surface over the durable store. It is
// READ + CONTROL only and purely additive: it exposes and pilots what discovery
// already armed (triggers, jobs, runs). It never changes YAML semantics — a
// runtime enable/disable is a live override that config discovery re-applies
// (authoritatively) from YAML on the next restart.
//
// Mounted under /ops (the caller StripPrefixes), so patterns here are relative.
func opsRoutes(st *store.Store, cfg OpsConfig) http.Handler {
	h := &opsAPI{st: st, rearm: cfg.Rearm, disarm: cfg.Disarm}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /triggers", h.listTriggers)
	mux.HandleFunc("POST /triggers", h.createTrigger)
	mux.HandleFunc("GET /triggers/{id}", h.getTrigger)
	mux.HandleFunc("POST /triggers/{id}/enable", h.enableTrigger(true))
	mux.HandleFunc("POST /triggers/{id}/disable", h.enableTrigger(false))
	mux.HandleFunc("GET /jobs", h.listJobs)
	mux.HandleFunc("GET /jobs/{id}", h.getJob)
	mux.HandleFunc("POST /jobs/{id}/replay", h.replayJob)
	mux.HandleFunc("GET /runs", h.listRuns)
	// Health surface: windowed metrics (success rate, durations, backlog), the
	// dead-letter queue (terminally-failed jobs), and trigger alerts (channels
	// failing repeatedly). Read-only; back the Overview + alerting.
	mux.HandleFunc("GET /metrics", h.metrics)
	mux.HandleFunc("GET /dlq", h.deadLetter)
	mux.HandleFunc("GET /alerts", h.alerts)
	// Session wake-ups (user-programmed schedules): bind a session to wake on a
	// schedule with an injected payload. Backed by the same cron-arm machinery.
	mux.HandleFunc("POST /schedules", h.createSchedule)
	mux.HandleFunc("GET /schedules", h.listSchedules)
	return withOpsAuth(cfg.Token, mux)
}

// withOpsAuth gates every ops route on a Bearer token when one is configured.
func withOpsAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtleConstEq(r.Header.Get("Authorization"), want) {
			next.ServeHTTP(w, r)
			return
		}
		writeOps(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	})
}

type opsAPI struct {
	st     *store.Store
	rearm  func(context.Context, CreateTriggerRequest) (store.Trigger, error)
	disarm func(context.Context, store.Trigger) error
}

// channelOpsAdapters are persistent-listener adapters whose message comes from
// each inbound event, so a trigger push carries no top-level message.
var channelOpsAdapters = map[string]bool{
	"discord": true, "telegram": true, "webhook": true, "rss": true, "whatsapp": true,
}

// ── Triggers ─────────────────────────────────────────────────────────────────

// createTrigger programs a trigger (cron) at runtime: it persists the trigger AND
// arms the live adapter, via the injected Rearm hook. 501 when no hook is wired.
func (a *opsAPI) createTrigger(w http.ResponseWriter, r *http.Request) {
	if a.rearm == nil {
		writeOps(w, http.StatusNotImplemented, map[string]any{"error": "runtime trigger arming is not enabled on this service"})
		return
	}
	var req CreateTriggerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOps(w, 400, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.Adapter == "" {
		req.Adapter = "cron"
	}
	// app_id + provider are always required. A top-level `message` is only needed
	// for adapters that fire with no inbound payload (a schedule). Channel
	// adapters (discord/telegram/…) get their message from each event, so they
	// carry no top-level message — don't reject them.
	if req.AppID == "" || req.Provider == "" {
		writeOps(w, 400, map[string]any{"error": "app_id and provider are required"})
		return
	}
	if req.Message == "" && req.Activation == nil && !channelOpsAdapters[req.Adapter] {
		writeOps(w, 400, map[string]any{"error": "message is required for this adapter"})
		return
	}
	t, err := a.rearm(r.Context(), req)
	if err != nil {
		writeOps(w, 422, map[string]any{"error": err.Error()})
		return
	}
	v := triggerView(t)
	v["armed"] = true
	v["note"] = "live for this process; add to the app YAML to persist across restarts"
	writeOps(w, http.StatusCreated, v)
}

// createSchedule programs a session wake-up: at the cron time, the bound session
// is re-launched (POST /messages) with the payload, AS Owner, with Context
// injected — the agent runs with its accumulated context + the payload. Backed by
// the same runtime cron-arm; Kind="schedule" so it lists on the schedule surface.
func (a *opsAPI) createSchedule(w http.ResponseWriter, r *http.Request) {
	if a.rearm == nil {
		writeOps(w, http.StatusNotImplemented, map[string]any{"error": "runtime scheduling is not enabled on this service"})
		return
	}
	var body struct {
		AppID       string                   `json:"app_id"`
		SessionID   string                   `json:"session_id"`
		Owner       string                   `json:"owner"`
		Schedule    string                   `json:"schedule"`
		Message     string                   `json:"message"`
		Context     string                   `json:"context"`
		Agent       string                   `json:"agent"`
		Reply       string                   `json:"reply"`
		Label       string                   `json:"label"`
		Reports     bool                     `json:"reports"`
		Attachments []channels.AttachmentRef `json:"attachments"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeOps(w, 400, map[string]any{"error": "invalid JSON body"})
		return
	}
	if body.AppID == "" || body.SessionID == "" || body.Schedule == "" || body.Message == "" {
		writeOps(w, 400, map[string]any{"error": "app_id, session_id, schedule and message are required"})
		return
	}
	req := CreateTriggerRequest{
		AppID: body.AppID, Provider: "sched-" + newID(), Adapter: "cron",
		Schedule: body.Schedule, Agent: body.Agent, Message: body.Message,
		Session: body.SessionID, Owner: body.Owner, Context: body.Context,
		Reply: body.Reply, Kind: "schedule", Reports: body.Reports,
		Attachments: body.Attachments,
	}
	t, err := a.rearm(r.Context(), req)
	if err != nil {
		writeOps(w, 422, map[string]any{"error": err.Error()})
		return
	}
	v := scheduleView(t)
	v["armed"] = true
	writeOps(w, http.StatusCreated, v)
}

func (a *opsAPI) listSchedules(w http.ResponseWriter, r *http.Request) {
	scheds, err := a.st.ListSchedules(r.Context(), r.URL.Query().Get("app"))
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(scheds))
	for _, t := range scheds {
		out = append(out, scheduleView(t))
	}
	writeOps(w, 200, map[string]any{"schedules": out, "count": len(out)})
}

func (a *opsAPI) listTriggers(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	enabledOnly := r.URL.Query().Get("enabled_only") == "true"
	trigs, err := a.st.AllTriggers(r.Context(), app, enabledOnly)
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(trigs))
	for _, t := range trigs {
		out = append(out, triggerView(t))
	}
	writeOps(w, 200, map[string]any{"triggers": out, "count": len(out)})
}

func (a *opsAPI) getTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := a.st.GetTrigger(r.Context(), id)
	if err != nil {
		writeOps(w, 404, map[string]any{"error": "trigger not found"})
		return
	}
	stat, _ := a.st.TriggerStats(r.Context(), id)
	runs, _ := a.st.ListRuns(r.Context(), store.RunFilter{TriggerID: id, Limit: 20})
	v := triggerView(t)
	v["stats"] = stat
	v["recent_runs"] = runViews(runs)
	writeOps(w, 200, v)
}

func (a *opsAPI) enableTrigger(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ok, err := a.st.SetTriggerEnabled(r.Context(), id, enabled)
		switch {
		case err != nil:
			writeOps(w, 500, map[string]any{"error": err.Error()})
			return
		case !ok:
			writeOps(w, 404, map[string]any{"error": "trigger not found"})
			return
		}
		disarmed := false
		if !enabled && a.disarm != nil {
			if t, gerr := a.st.GetTrigger(r.Context(), id); gerr == nil {
				disarmed = a.disarm(r.Context(), t) == nil
			}
		}
		writeOps(w, 200, map[string]any{"id": id, "enabled": enabled, "disarmed": disarmed,
			"note": "runtime override; YAML config is re-applied on restart"})
	}
}

// ── Jobs ─────────────────────────────────────────────────────────────────────

func (a *opsAPI) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	appID := q.Get("app_id")
	if appID == "" {
		appID = q.Get("app")
	}
	jobs, err := a.st.ListJobs(r.Context(), store.JobFilter{
		AppID: appID, TriggerID: q.Get("trigger"),
		State: store.JobState(q.Get("state")), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobView(j))
	}
	writeOps(w, 200, map[string]any{"jobs": out, "count": len(out)})
}

func (a *opsAPI) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := a.st.Get(r.Context(), id)
	if err != nil {
		writeOps(w, 404, map[string]any{"error": "job not found"})
		return
	}
	runs, _ := a.st.ListRuns(r.Context(), store.RunFilter{JobID: id, Limit: 100})
	v := jobView(j)
	v["runs"] = runViews(runs) // the full per-attempt execution report
	writeOps(w, 200, v)
}

func (a *opsAPI) replayJob(w http.ResponseWriter, r *http.Request) {
	ok, err := a.st.ReplayJob(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		writeOps(w, 500, map[string]any{"error": err.Error()})
	case !ok:
		writeOps(w, 409, map[string]any{"error": "job not found or not in a replayable (done/failed) state"})
	default:
		writeOps(w, 200, map[string]any{"id": r.PathValue("id"), "state": "pending", "replayed": true})
	}
}

// ── Runs ─────────────────────────────────────────────────────────────────────

func (a *opsAPI) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	// Accept both `app_id` (the daemon proxy's canonical name) and the short
	// `app` alias — they previously disagreed, so the filter silently no-op'd
	// and every app saw the whole run table.
	appID := q.Get("app_id")
	if appID == "" {
		appID = q.Get("app")
	}
	runs, err := a.st.ListRuns(r.Context(), store.RunFilter{
		AppID: appID, TriggerID: q.Get("trigger"), JobID: q.Get("job"),
		Outcome: q.Get("outcome"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeOps(w, 200, map[string]any{"runs": runViews(runs), "count": len(runs)})
}

// ── Health (metrics / DLQ / alerts) ──────────────────────────────────────────

// windowSince parses a ?window duration (e.g. "24h", "1h", "7d-ish as 168h")
// into a start time. Defaults to 24h; caps at 30d so a query can't scan forever.
func windowSince(q string) (time.Time, string) {
	d := 24 * time.Hour
	if q != "" {
		if parsed, err := time.ParseDuration(q); err == nil && parsed > 0 {
			d = parsed
		}
	}
	if d > 30*24*time.Hour {
		d = 30 * 24 * time.Hour
	}
	return time.Now().UTC().Add(-d), d.String()
}

func (a *opsAPI) metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	appID := q.Get("app_id")
	if appID == "" {
		appID = q.Get("app")
	}
	since, window := windowSince(q.Get("window"))
	m, err := a.st.MetricsWindow(r.Context(), appID, since)
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	m.Window = window
	writeOps(w, 200, m)
}

func (a *opsAPI) deadLetter(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	appID := q.Get("app_id")
	if appID == "" {
		appID = q.Get("app")
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	jobs, err := a.st.DeadLetter(r.Context(), appID, limit)
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobView(j))
	}
	writeOps(w, 200, map[string]any{"dlq": out, "count": len(out)})
}

func (a *opsAPI) alerts(w http.ResponseWriter, r *http.Request) {
	streak, _ := strconv.Atoi(r.URL.Query().Get("streak"))
	if streak <= 0 {
		streak = defaultAlertStreak
	}
	alerts, err := a.st.TriggerAlerts(r.Context(), streak)
	if err != nil {
		writeOps(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeOps(w, 200, map[string]any{"alerts": alerts, "count": len(alerts), "streak": streak})
}

// ── Views ────────────────────────────────────────────────────────────────────

func triggerView(t store.Trigger) map[string]any {
	v := map[string]any{
		"id": t.ID, "app_id": t.AppID, "provider": t.Provider, "adapter": t.Adapter,
		"enabled": t.Enabled, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
	if t.Cursor != "" {
		v["cursor"] = t.Cursor
	}
	if s := cfgString(t.ConfigJSON, "activation", "owner"); s != "" {
		v["owner"] = s
	}
	if t.Adapter == "cron" {
		if next := cronNextRun(t.ConfigJSON); next != nil {
			v["next_run"] = next
		}
	}
	return v
}

// scheduleView renders a session wake-up for the ops surface: the bound session,
// owner, payload excerpt, schedule and next_run — extracted from the stored
// trigger config (a TriggerSpec whose Activation binds the session).
func scheduleView(t store.Trigger) map[string]any {
	v := map[string]any{
		"id": t.ID, "app_id": t.AppID, "enabled": t.Enabled,
		"created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
	if s := cfgString(t.ConfigJSON, "schedule"); s != "" {
		v["schedule"] = s
		if next := cronNextRun(t.ConfigJSON); next != nil {
			v["next_run"] = next
		}
	}
	if s := cfgString(t.ConfigJSON, "activation", "session"); s != "" {
		v["session_id"] = s
	}
	if s := cfgString(t.ConfigJSON, "activation", "owner"); s != "" {
		v["owner"] = s
	}
	if s := cfgString(t.ConfigJSON, "activation", "message"); s != "" {
		v["message"] = s
	}
	return v
}

// cfgString walks a decoded trigger config by key path and returns a string leaf,
// matching keys case-insensitively (ActivationConfig serializes with Go field
// names). Returns "" on any miss.
func cfgString(configJSON string, keys ...string) string {
	var m map[string]any
	if configJSON == "" || json.Unmarshal([]byte(configJSON), &m) != nil {
		return ""
	}
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		v, found := mm[k]
		if !found {
			for kk, vv := range mm {
				if strings.EqualFold(kk, k) {
					v, found = vv, true
					break
				}
			}
		}
		if !found {
			return ""
		}
		cur = v
	}
	s, _ := cur.(string)
	return s
}

func newID() string { return uuid.NewString()[:8] }

func jobView(j store.Job) map[string]any {
	v := map[string]any{
		"id": j.ID, "app_id": j.AppID, "trigger_id": j.TriggerID, "provider": j.Provider,
		"state": j.State, "attempts": j.Attempts, "dedup_key": j.DedupKey,
		"created_at": j.CreatedAt, "updated_at": j.UpdatedAt,
	}
	if j.LastError != "" {
		v["last_error"] = j.LastError
	}
	return v
}

func runViews(runs []store.Run) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		v := map[string]any{
			"id": r.ID, "job_id": r.JobID, "app_id": r.AppID, "trigger_id": r.TriggerID,
			"provider": r.Provider, "adapter": r.Adapter, "attempt": r.Attempt,
			"outcome": r.Outcome, "duration_ms": r.DurationMs,
			"started_at": r.StartedAt, "ended_at": r.EndedAt,
		}
		if r.SessionID != "" {
			v["session_id"] = r.SessionID
		}
		if r.ReplyChars > 0 {
			v["reply_chars"] = r.ReplyChars
			v["reply_preview"] = r.ReplyPreview
		}
		if r.Error != "" {
			v["error"] = r.Error
		}
		out = append(out, v)
	}
	return out
}

// cronNextRun best-effort computes a cron trigger's next fire time from a
// "schedule" expression found anywhere in its persisted config. Returns nil when
// the schedule is not stored or is unparseable — the rest of the report is
// unaffected (the schedule lives in the boot-time adapter config; persisting it
// into the trigger is the only thing needed to always populate this).
func cronNextRun(configJSON string) *time.Time {
	if configJSON == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(configJSON), &m) != nil {
		return nil
	}
	expr := findSchedule(m)
	if expr == "" {
		return nil
	}
	sched, err := cron.Parse(expr)
	if err != nil {
		return nil
	}
	next := sched.Next(time.Now().UTC())
	if next.IsZero() {
		return nil
	}
	return &next
}

// findSchedule walks a decoded config map for a string "schedule" value.
func findSchedule(m map[string]any) string {
	for k, v := range m {
		if strings.EqualFold(k, "schedule") {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		if sub, ok := v.(map[string]any); ok {
			if s := findSchedule(sub); s != "" {
				return s
			}
		}
	}
	return ""
}

func writeOps(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// subtleConstEq is a length-independent constant-time-ish string compare for the
// bearer token (avoids a trivial timing oracle on the credential).
func subtleConstEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := 0; i < len(a); i++ {
		d |= a[i] ^ b[i]
	}
	return d == 0
}
