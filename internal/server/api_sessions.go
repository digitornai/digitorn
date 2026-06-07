package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

// turnSafetyCutoff bounds an agent turn end-to-end even when the LLM
// client's own timeout is missing or too generous. It is an emergency
// circuit-breaker, not a budget — real timeouts live on cfg.Workers.LLM.
const turnSafetyCutoff = 5 * time.Minute

// ---------- list / create / search ----------

type sessionSummary struct {
	SessionID   string `json:"session_id"`
	AppID       string `json:"app_id"`
	UserID      string `json:"user_id"`
	Title       string `json:"title,omitempty"`
	LastSeq     uint64 `json:"last_seq"`
	EventCount  uint64 `json:"event_count"`
	StartedAt   string `json:"started_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	Closed      bool   `json:"closed,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	// Preview is a short snippet of the session's first user message, so a list
	// can label a session by what it's about instead of a generic/echoed title.
	Preview string `json:"preview,omitempty"`
}

func (d *Daemon) listSessions(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	limit := parseIntQuery(r, "limit", 50)
	offset := parseIntQuery(r, "offset", 0)

	summaries, err := d.walkSessionMeta(appID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].UpdatedAt > summaries[j].UpdatedAt
	})
	total := len(summaries)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": summaries[offset:end],
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

func (d *Daemon) searchSessions(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": []sessionSummary{}, "total": 0})
		return
	}
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	summaries, err := d.walkSessionMeta(appID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search_failed", err.Error())
		return
	}
	out := summaries[:0]
	for _, s := range summaries {
		if strings.Contains(strings.ToLower(s.Title), q) ||
			strings.Contains(strings.ToLower(s.SessionID), q) {
			out = append(out, s)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "total": len(out)})
}

type createSessionRequest struct {
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Workdir   string `json:"workdir,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Web clients open a chat by creating the session AND sending the first
	// message in one call. When Message is set we append it and start the turn,
	// exactly as a follow-up POST /messages would.
	Message         string `json:"message,omitempty"`
	ClientMessageID string `json:"client_message_id,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Skill           string `json:"skill,omitempty"`
	// EntryAgent pins which of the app's agents handles this session, and Context
	// is extra system-prompt text injected for it. Both are optional passthroughs
	// for non-human launchers (e.g. a background channel trigger); empty for human
	// clients, which keeps behaviour byte-identical to before.
	EntryAgent string `json:"entry_agent,omitempty"`
	Context    string `json:"context,omitempty"`
}

func (d *Daemon) createSession(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())

	var req createSessionRequest
	if r.ContentLength > 0 {
		if err := readJSONLenient(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	sid := req.SessionID
	if sid == "" {
		sid = uuid.NewString()
	}

	// WD : resolve the agent workdir per the app's workdir_mode. A `required`
	// app with no supplied workdir is rejected with workdir_required (no
	// session created). The managed default dir is created lazily ONLY when
	// nothing is supplied — never otherwise (no dead directories).
	resolvedWD, err := d.resolveWorkdir(r.Context(), appID, userID, sid, req.Workdir)
	if err != nil {
		if errors.Is(err, workdir.ErrWorkdirRequired) {
			writeError(w, http.StatusBadRequest, "workdir_required",
				"this app requires a working directory; pass `workdir` when creating the session")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_workdir", err.Error())
		return
	}

	ev := sessionstore.Event{
		Type:      sessionstore.EventSessionStarted,
		SessionID: sid,
		AppID:     appID,
		UserID:    userID,
		Meta: &sessionstore.MetaPayload{
			Title:        req.Title,
			Workspace:    req.Workspace,
			Workdir:      resolvedWD,
			EntryAgent:   req.EntryAgent,
			ContextExtra: req.Context,
		},
	}
	ctxApp, cancelApp := appendCtx(r.Context())
	defer cancelApp()
	seq, err := d.sessionStore.AppendDurable(ctxApp, ev)
	if err != nil {
		writeError(w, appendErrStatus(err), "append_failed", err.Error())
		return
	}
	// Publish meta.json so the list endpoint can find this session without
	// having to wait for the flusher to commit the JSONL.
	if err := d.sessionStore.SyncMetaToDisk(sid); err != nil {
		d.logger.Warn("createSession: sync meta failed",
			"sid", sid, "err", err.Error())
	}
	// Snapshot the workspace's starting state (git baseline) in the background
	// so the agent's later changes diff cleanly against it. Never blocks.
	d.baselineWorkspaceAsync(resolvedWD)
	// Inline first message : web clients send it with the create call. Append
	// the user message and kick the turn, exactly as POST /messages would.
	var firstMsgSeq uint64
	if req.Message != "" && d.engine != nil {
		mseq, err := d.sessionStore.AppendDurable(ctxApp, sessionstore.Event{
			Type:      sessionstore.EventUserMessage,
			SessionID: sid,
			AppID:     appID,
			UserID:    userID,
			Message:   &sessionstore.MessagePayload{Role: "user", Content: req.Message, ClientMessageID: req.ClientMessageID},
		})
		if err != nil {
			writeError(w, appendErrStatus(err), "append_failed", err.Error())
			return
		}
		firstMsgSeq = mseq
		d.runTurnAsync(r.Context(), sid, appID, userID, extractBearer(r), req.Mode, req.Skill)
	}

	resp := map[string]any{
		"session_id":  sid,
		"app_id":      appID,
		"user_id":     userID,
		"seq":         seq,
		"started_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"title":       req.Title,
		"workspace":   req.Workspace,
		"workdir":     resolvedWD,
		"instance_id": d.envelopeBuilder.InstanceID,
	}
	if firstMsgSeq > 0 {
		resp["first_message"] = map[string]any{"seq": firstMsgSeq}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// resolveWorkdir resolves a session's workdir from the app's workdir_mode +
// runtime.workdir and the client-supplied workdir, creating the managed
// default dir only when nothing is supplied. Returns workdir.ErrWorkdirRequired
// when the app requires a workdir but none was given.
func (d *Daemon) resolveWorkdir(ctx context.Context, appID, userID, sid, userWorkdir string) (string, error) {
	mode := workdir.ModeAuto
	var fixed string
	if d.appMgr != nil {
		if rt, err := d.appMgr.Get(ctx, appID); err == nil && rt != nil && rt.Definition != nil && rt.Definition.Runtime != nil {
			mode = workdir.NormalizeMode(string(rt.Definition.Runtime.WorkdirMode))
			fixed = rt.Definition.Runtime.Workdir
		}
	}
	return workdir.Resolve(workdir.Request{
		Mode:        mode,
		FixedPath:   fixed,
		UserWorkdir: userWorkdir,
		AppID:       appID,
		UserID:      userID,
		SessionID:   sid,
	})
}

func (d *Daemon) getSession(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	state.RLock()
	defer state.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    state.SessionID,
		"app_id":        state.AppID,
		"user_id":       state.UserID,
		"title":         state.Title,
		"workspace":     state.Workspace,
		"workdir":       state.Workdir,
		"first_seq":     state.FirstSeq,
		"last_seq":      state.LastSeq,
		"event_count":   state.EventCount,
		"message_count": len(state.Messages),
		"started_at":    isoNano(state.StartedAtNano),
		"ended_at":      isoNano(state.EndedAtNano),
		"closed":        state.Closed,
		"interrupted":   state.Interrupted,
		"turn_count":    state.TurnCount,
		"tokens_in":     state.TokensIn,
		"tokens_out":    state.TokensOut,
		"usd_total":     state.UsdTotal,
		"partial":       state.Partial,
		// active_mode lets the composer's mode picker restore the session's
		// last-active mode on reload instead of falling back to the app default.
		"active_mode": state.ActiveMode,
		"instance_id": d.envelopeBuilder.InstanceID,
	})
}

func (d *Daemon) deleteSession(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	// session_end : fire the lifecycle hook BEFORE teardown so a cleanup
	// / notify / summary hook can still run against the live session.
	if d.lifecycle != nil {
		snap := state.Snapshot()
		d.lifecycle.FireLifecycle(r.Context(), schema.HookEventSessionEnd, snap.AppID, sid, snap.UserID)
	}
	// Drop the FD then remove the dir; finally evict the in-memory state.
	d.sessionStore.DropFD(sid)
	dir := d.sessionPaths.SessionDir(sid)
	if err := os.RemoveAll(dir); err != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	d.sessionStore.Drop(sid)
	if d.sessionRunner != nil {
		d.sessionRunner.Forget(sid)
	}
	// Drop the context-breakdown parts (system prompt + tool schemas) recorded
	// for the background recount, so the map doesn't leak per deleted session.
	d.ctxParts.Delete(sid)
	// Drop the freshest context variable for this session (memory hygiene).
	if d.contextTracker != nil {
		d.contextTracker.Delete(sid)
	}
	// Drop the session's in-memory behavioral state (counters/sets/flags).
	if bc, ok := d.engine.(interface{ CleanupBehaviorSession(appID, sid string) }); ok {
		bc.CleanupBehaviorSession(chi.URLParam(r, "app_id"), sid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "deleted": true})
}

// ---------- history / events / state / memory ----------

func (d *Daemon) getHistory(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	jres, err := sessionstore.ReadJSONL(d.sessionPaths.EventsFile(sid), sessionstore.JSONLBestEffort, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	since := parseUint64Query(r, "since", 0)
	limit := parseIntQuery(r, "limit", 0)
	// The transcript (Messages) is the LOSSLESS full history rebuilt from disk
	// (storage snapshot + JSONL message events), independent of the live
	// in-memory state — whose message slice is bounded to the model's window.
	// Durable-append (fsync before projection) guarantees disk is never behind.
	snap, _, _ := sessionstore.ReadSnapshot(d.sessionPaths.SessionDir(sid))
	transcript := sessionstore.TranscriptFromParts(snap, jres.Events)
	resp := sessionstore.BuildHistory(state, transcript, jres.Events, sessionstore.ViewOptions{
		IncludePayload: true,
		StartSeq:       since,
		MaxEvents:      limit,
		InstanceID:     d.envelopeBuilder.InstanceID,
	})
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) getEvents(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	jres, err := sessionstore.ReadJSONL(d.sessionPaths.EventsFile(sid), sessionstore.JSONLBestEffort, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	since := parseUint64Query(r, "since", 0)
	limit := parseIntQuery(r, "limit", 0)
	resp := sessionstore.BuildEvents(jres.Events, sessionstore.ViewOptions{
		IncludePayload: true,
		StartSeq:       since,
		MaxEvents:      limit,
	})
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) getState(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	snap := state.Snapshot()
	writeJSON(w, http.StatusOK, snap)
}

func (d *Daemon) getMemory(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	state.RLock()
	mem := make(map[string]string, len(state.Memory))
	for k, v := range state.Memory {
		mem[k] = v
	}
	facts := append([]string(nil), state.Facts...)
	goal := state.Goal
	state.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     sid,
		"memory":         mem,
		"semantic_facts": facts,
		"goal":           goal,
	})
}

func (d *Daemon) getAgents(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	state.RLock()
	children := append([]sessionstore.ChildAgent(nil), state.Children...)
	state.RUnlock()
	resp := map[string]any{
		"session_id": sid,
		// children : the DURABLE, replayable agent tree projected from
		// agent_spawn / agent_result events — what a reconnecting client
		// reconstructs via "since seq" replay.
		"children": children,
		"count":    len(children),
	}
	// live : the AgentManager's real-time view (status + per-agent telemetry :
	// tool_calls, llm_calls, tokens, depth). Latest-value-wins, so a client
	// resyncs the current numbers without replaying every delta.
	if d.agents != nil {
		resp["live"] = d.agents.List(sid)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) getQueue(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	// V1: the pending queue is always empty — there is no runtime yet to
	// buffer messages between turns. The shape stays consistent with Python.
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"queue":      []any{},
		"size":       0,
	})
}

// ---------- messages / abort / compact ----------

type postMessageRequest struct {
	Role        string                 `json:"role,omitempty"`
	Content     string                 `json:"content,omitempty"`
	Text        string                 `json:"text,omitempty"`
	Message     string                 `json:"message,omitempty"` // web-client name for content
	ToolCallIDs []string               `json:"tool_call_ids,omitempty"`
	Attachments []sessionstore.BlobRef `json:"attachments,omitempty"`
	// ClientMessageID is echoed back on the user_message event so a client can
	// reconcile its optimistic bubble deterministically (no content matching).
	ClientMessageID string `json:"client_message_id,omitempty"`
	// Mode is the composer mode the user picked for this message
	// (runtime.modes). Empty → the session's sticky mode / app default.
	Mode string `json:"mode,omitempty"`
	// Skill is the /use_skill command the user prefixed this message with
	// (e.g. "/commit"). Non-empty → the engine injects the skill's instructions
	// as a forced system directive for this turn.
	Skill string `json:"skill,omitempty"`
}

func (d *Daemon) postMessage(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	appID := chi.URLParam(r, "app_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	_ = state

	var req postMessageRequest
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	content := req.Content
	if content == "" {
		content = req.Text
	}
	if content == "" {
		content = req.Message
	}
	if content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content, text or message required")
		return
	}
	role := req.Role
	if role == "" {
		role = "user"
	}
	ev := sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: sid,
		AppID:     appID,
		UserID:    userIDOf(r.Context()),
		Message: &sessionstore.MessagePayload{
			Role:            role,
			Content:         content,
			ClientMessageID: req.ClientMessageID,
			ToolCallIDs:     req.ToolCallIDs,
			Attachments:     req.Attachments,
		},
	}
	ctxApp, cancelApp := appendCtx(r.Context())
	defer cancelApp()
	seq, err := d.sessionStore.AppendDurable(ctxApp, ev)
	if err != nil {
		writeError(w, appendErrStatus(err), "append_failed", err.Error())
		return
	}
	// A new message changed the context : refresh the exact gauge NOW (per-session,
	// non-blocking) so the occupancy is up to date the instant the message lands,
	// not only once the turn builds its first request. sid is this session's own id.
	d.touchContext(sid)

	// User messages trigger the agent turn. Assistant / system / tool
	// messages are write-throughs from upstream callers and must NOT
	// re-enter the runtime (assistant in particular would loop).
	if role == "user" && d.engine != nil && appID != "" {
		d.runTurnAsync(r.Context(), sid, appID, userIDOf(r.Context()), extractBearer(r), req.Mode, req.Skill)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id": sid,
		"seq":        seq,
		"role":       role,
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// runTurnAsync schedules the agent turn through the session runner, which
// guarantees at most one turn per session at a time (coalescing concurrent
// triggers) and detaches from the HTTP request. The same runner serves
// proactive wakes (background completion, watchers, cron), so a user
// message and a proactive wake can never run two turns on one session at
// once. The assistant reply reaches the client via the session-store →
// Socket.IO bridge ; errors are logged inside the runner.
func (d *Daemon) runTurnAsync(_ context.Context, sid, appID, userID, userJWT, mode, skill string) {
	if d.sessionRunner == nil {
		return
	}
	d.sessionRunner.WakeTurn(runtime.TurnInput{
		AppID:     appID,
		SessionID: sid,
		UserID:    userID,
		UserJWT:   userJWT,
		Mode:      mode,
		Skill:     skill,
	})
}

func (d *Daemon) abortTurn(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	appID := chi.URLParam(r, "app_id")
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	seq, stopped, err := d.abortSession(r.Context(), sid, appID, userIDOf(r.Context()))
	if err != nil {
		writeError(w, appendErrStatus(err), "append_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "seq": seq, "interrupted": true, "stopped": stopped,
	})
}

// abortSession is the TOTAL session-stop primitive : it halts everything alive
// in the session in one shot, then records the durable interruption marker.
//
// In order :
//  1. interrupt the in-flight turn (cancel its context → the engine unwinds at
//     the next iteration / on the live LLM call, stops streaming promptly, and
//     persists the partial answer + the turn_ended=interrupted terminal event),
//  2. cancel the whole delegated agent tree (sub-agents run on independent
//     contexts, so the turn's own cancel can't reach them),
//  3. cancel every running background task in the session,
//  4. append the durable EventSessionInterrupt so a reconnecting / cold-loading
//     client sees the session was stopped.
//
// Every step is best-effort and idempotent — calling it with nothing running is
// a harmless no-op. Returns the marker's seq and a small per-surface tally.
func (d *Daemon) abortSession(ctx context.Context, sid, appID, userID string) (uint64, map[string]any, error) {
	turnStopped := d.sessionRunner.Abort(sid)
	agentsStopped := 0
	if d.agents != nil {
		agentsStopped = d.agents.CancelAll(sid)
	}
	bgStopped := 0
	if d.background != nil {
		bgStopped = d.background.CancelAllForSession(sid)
	}

	ctxApp, cancelApp := appendCtx(ctx)
	defer cancelApp()
	seq, err := d.sessionStore.AppendDurable(ctxApp, sessionstore.Event{
		Type:      sessionstore.EventSessionInterrupt,
		SessionID: sid,
		AppID:     appID,
		UserID:    userID,
	})
	stopped := map[string]any{
		"turn":       turnStopped,
		"agents":     agentsStopped,
		"background": bgStopped,
	}
	return seq, stopped, err
}

func (d *Daemon) compactSession(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	// pre_compact : fire right before compaction (doc semantics). A hook
	// can log / notify / inject a summary marker before the cutoff runs.
	if d.lifecycle != nil {
		snap := state.Snapshot()
		d.lifecycle.FireLifecycle(r.Context(), schema.HookEventPreCompact, snap.AppID, sid, snap.UserID)
	}
	c := d.sessionStore.Compactor(sessionstore.CompactorConfig{})
	res, err := c.Compact(r.Context(), state, sessionstore.CompactOptions{
		TruncateMode: sessionstore.TruncateSync,
		Gate:         d.sessionStore,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "compact_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":       res.SessionID,
		"cutoff_seq":       res.CutoffSeq,
		"compact_done_seq": res.CompactDoneSeq,
		"snapshot_sha256":  res.SnapshotSHA256,
		"binary":           res.SnapshotFormat == sessionstore.SnapshotBinary,
		"events_compacted": res.EventsCompacted,
		"bytes_before":     res.JSONLBytesBefore,
		"duration_ms":      (res.EndedAtUnixNano - res.StartedAtUnixNano) / int64(time.Millisecond),
	})
}

// ---------- helpers ----------

// requireOwnedSession loads the session and enforces user ownership.
// Returns the SessionState if the requestor owns it.
var errSessionNotFound = errors.New("session not found")
var errSessionForbidden = errors.New("session does not belong to this user")

func (d *Daemon) requireOwnedSession(ctx context.Context, sid string) (*sessionstore.SessionState, error) {
	if sid == "" {
		return nil, errSessionNotFound
	}
	state, err := d.sessionStore.State(sid)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	state.RLock()
	owner := state.UserID
	first := state.FirstSeq
	state.RUnlock()
	if first == 0 {
		// Session never received any event — treat as not found.
		return nil, errSessionNotFound
	}
	userID := userIDOf(ctx)
	if d.cfg != nil && d.cfg.Auth.Enabled {
		// Auth ON : fail-CLOSED. The caller MUST present a concrete user id
		// that matches the session owner — an empty / "anonymous" id never
		// grants access (the previous check skipped enforcement whenever
		// userID was empty, letting an unauthenticated request read/delete/
		// abort any session).
		if userID == "" || userID == "anonymous" || userID != owner {
			return nil, errSessionForbidden
		}
	} else if owner != "" && userID != "" && userID != owner {
		// Auth OFF (dev mode) : best-effort ownership check only when both
		// ids are present ; no hard requirement.
		return nil, errSessionForbidden
	}
	return state, nil
}

func errStatus(err error) int {
	switch {
	case errors.Is(err, errSessionNotFound):
		return http.StatusNotFound
	case errors.Is(err, errSessionForbidden):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func errCode(err error) string {
	switch {
	case errors.Is(err, errSessionNotFound):
		return "session_not_found"
	case errors.Is(err, errSessionForbidden):
		return "forbidden"
	}
	return "internal_error"
}

func isoNano(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

// walkSessionMeta scans the entire sessions tree, reading meta.json to
// build summaries filtered by app_id and user_id. O(N) over all sessions —
// fine for V1 development workloads. V2 will add an index.
func (d *Daemon) walkSessionMeta(appID, userID string) ([]sessionSummary, error) {
	root := d.sessionPaths.Root
	// Phase 1 — collect candidate meta.json paths. Sub-agent transcript dirs
	// (<sid>::agent::<run>, percent-escaped on disk) are internal delegation logs,
	// never user conversations : skip them by DIR NAME so we never even read their
	// meta. With heavy delegation there are hundreds of these, and reading every one
	// just to drop it was pure waste.
	var paths []string
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() != "meta.json" {
			return nil
		}
		dirName := filepath.Base(filepath.Dir(path))
		if _, _, isSub := sessionstore.SubAgentSession(sessionstore.DecodeSessionDir(dirName)); isSub {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Phase 2 — read + parse in parallel. The cost is per-file I/O (meta read, plus
	// a one-time events scan for un-previewed sessions), so a bounded fan-out cuts
	// wall-time on a large store from many seconds to ~1.
	summaries := make([]*sessionSummary, len(paths))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			summaries[i] = d.readSessionSummary(paths[i], appID, userID)
		}(i)
	}
	wg.Wait()
	out := make([]sessionSummary, 0, len(paths))
	for _, s := range summaries {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

// readSessionSummary turns one session's meta.json into a summary, or nil to skip
// (wrong app/user, empty shell, unreadable). Fast path : a cached preview lists
// straight from meta. Heal path : no preview → scan the events file once and write
// the derived preview + counters back so the next list is cheap. Safe to call from
// many goroutines : each touches a distinct session directory.
func (d *Daemon) readSessionSummary(path, appID, userID string) *sessionSummary {
	dir := filepath.Dir(path)
	meta, err := sessionstore.ReadMeta(path)
	if err != nil || meta == nil {
		return nil
	}
	if appID != "" && meta.AppID != "" && meta.AppID != appID {
		return nil
	}
	if userID != "" && meta.UserID != "" && meta.UserID != userID {
		return nil
	}
	// The session ID is the authoritative value from meta.json ; fall back to the
	// decoded dir name for any legacy session whose meta predates the field.
	sid := meta.SessionID
	if sid == "" {
		sid = sessionstore.DecodeSessionDir(filepath.Base(dir))
	}
	if _, _, isSub := sessionstore.SubAgentSession(sid); isSub {
		return nil
	}
	if meta.Preview != "" {
		return &sessionSummary{
			SessionID:  sid,
			AppID:      meta.AppID,
			UserID:     meta.UserID,
			Title:      meta.Title,
			LastSeq:    meta.LastSeq,
			EventCount: meta.EventCount,
			StartedAt:  isoNano(meta.StartedAtNano),
			UpdatedAt:  isoNano(meta.UpdatedAtNano),
			Workspace:  meta.Workspace,
			Workdir:    meta.Workdir,
			Preview:    meta.Preview,
		}
	}
	scan := scanSessionStats(d.sessionPaths.EventsFile(sid))
	if !scan.hasUser && scan.events <= 1 && meta.EventCount <= 1 {
		return nil // empty shell : no conversation to show
	}
	if meta.EventCount < scan.events {
		meta.EventCount = scan.events
	}
	if meta.LastSeq < scan.lastSeq {
		meta.LastSeq = scan.lastSeq
	}
	if scan.lastTsNano > 0 {
		meta.UpdatedAtNano = scan.lastTsNano
	}
	meta.Preview = scan.preview
	// Best-effort self-heal : a failed write just means the next list re-scans.
	_ = sessionstore.WriteMetaAtomic(dir, meta, false)
	return &sessionSummary{
		SessionID:  sid,
		AppID:      meta.AppID,
		UserID:     meta.UserID,
		Title:      meta.Title,
		LastSeq:    meta.LastSeq,
		EventCount: meta.EventCount,
		StartedAt:  isoNano(meta.StartedAtNano),
		UpdatedAt:  isoNano(meta.UpdatedAtNano),
		Workspace:  meta.Workspace,
		Workdir:    meta.Workdir,
		Preview:    meta.Preview,
	}
}

// sessionScan is the per-session view the list endpoint derives from the events
// log in a single pass : enough to label the session (preview), order it
// (lastTsNano), size it (events/lastSeq), and tell a real conversation from an
// empty shell (hasUser).
type sessionScan struct {
	preview    string // first user message, collapsed + capped
	events     uint64 // total persisted events (line count)
	lastSeq    uint64 // seq of the last event
	lastTsNano int64  // ts of the last event (for "updated at")
	hasUser    bool   // at least one user_message → a real conversation
}

// scanSessionStats reads events.jsonl once and derives sessionScan. It counts
// every line, captures the first user_message for the preview, and decodes the
// last line for seq/ts — two JSON decodes total, the rest is a cheap byte scan.
// Authoritative where meta.json lies (a frozen event_count). Zero value on any
// read error (treated as an empty/unreadable session by the caller).
func scanSessionStats(eventsPath string) sessionScan {
	f, err := os.Open(eventsPath)
	if err != nil {
		return sessionScan{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long event lines
	userMarker := []byte(`"type":"user_message"`)
	var res sessionScan
	var lastLine []byte
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		res.events++
		lastLine = append(lastLine[:0], line...) // keep a copy : the scanner reuses its buffer
		if !res.hasUser && bytes.Contains(line, userMarker) {
			var ev sessionstore.Event
			if json.Unmarshal(line, &ev) == nil && ev.Type == sessionstore.EventUserMessage && ev.Message != nil {
				res.hasUser = true
				res.preview = previewText(ev.Message)
			}
		}
	}
	if len(lastLine) > 0 {
		var ev sessionstore.Event
		if json.Unmarshal(lastLine, &ev) == nil {
			res.lastSeq = ev.Seq
			res.lastTsNano = ev.TsUnixNano
		}
	}
	return res
}

// previewText flattens a user message to a single capped line for the picker.
func previewText(msg *sessionstore.MessagePayload) string {
	txt := msg.Content
	if txt == "" {
		for _, p := range msg.Parts {
			txt += p.Text
		}
	}
	txt = strings.Join(strings.Fields(txt), " ") // collapse whitespace/newlines
	if r := []rune(txt); len(r) > 80 {
		txt = string(r[:79]) + "…"
	}
	return txt
}
