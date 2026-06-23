package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// renameSession updates a session's title after creation. The title is otherwise
// only set at creation (EventSessionStarted), so we append a durable
// EventSessionRenamed that the projection applies — surviving replay/cold-load —
// and sync the meta so the session list reflects it immediately.
func (d *Daemon) renameSession(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())

	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}

	ctxApp, cancel := appendCtx(r.Context())
	defer cancel()
	if _, err := d.sessionStore.AppendDurable(ctxApp, sessionstore.Event{
		Type:      sessionstore.EventSessionRenamed,
		SessionID: sid,
		AppID:     appID,
		UserID:    userID,
		Meta:      &sessionstore.MetaPayload{Title: title},
	}); err != nil {
		writeError(w, appendErrStatus(err), "append_failed", err.Error())
		return
	}
	if err := d.sessionStore.SyncMetaToDisk(sid); err != nil {
		d.logger.Warn("renameSession: sync meta failed", "sid", sid, "err", err.Error())
	}

	state.RLock()
	wd := state.Workdir
	startedNano := state.StartedAtNano
	state.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"title":      title,
		"workdir":    wd,
		"started_at": time.Unix(0, startedNano).UTC().Format(time.RFC3339Nano),
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// forkSession clones a session's conversation into a fresh session. In an
// event-sourced store a fork is just "replay the durable log under a new id" :
// we copy every durable event verbatim (re-stamped to the new session, seqs
// re-assigned by the bus), so the fork carries the full conversation — user/
// assistant messages, tool calls AND their results — at byte fidelity. The
// source's own session_started is dropped in favour of a fresh one carrying a
// "Fork of …" title plus the copied workspace/workdir binding (the workspace
// FILES are a separate concern, not duplicated here).
//
// A source that was context-compacted keeps only its post-cutoff events on
// disk (the rest lives in the snapshot), so the fork begins at that cutoff —
// the response flags `partial_history` rather than silently dropping the head.
func (d *Daemon) forkSession(w http.ResponseWriter, r *http.Request) {
	srcSid := sessionIDParam(r)
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())

	src, err := d.requireOwnedSession(r.Context(), srcSid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	src.RLock()
	srcTitle := src.Title
	workspace := src.Workspace
	workdir := src.Workdir
	src.RUnlock()

	// Optional point-fork : clone only events strictly BEFORE this source seq, so
	// the new session rewinds to just before that message (the client then re-fills
	// the composer with it). Absent / 0 → clone the full conversation (unchanged).
	var beforeSeq uint64
	if v := r.URL.Query().Get("before_seq"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			beforeSeq = n
		}
	}

	jres, err := sessionstore.ReadJSONL(d.sessionPaths.EventsFile(srcSid), sessionstore.JSONLBestEffort, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}

	newSid := uuid.NewString()
	title := "Fork of " + forkLabel(srcTitle, srcSid)

	evs := make([]sessionstore.Event, 0, len(jres.Events)+1)
	evs = append(evs, sessionstore.Event{
		Type:      sessionstore.EventSessionStarted,
		SessionID: newSid,
		AppID:     appID,
		UserID:    userID,
		Meta: &sessionstore.MetaPayload{
			Title:     title,
			Workspace: workspace,
			Workdir:   workdir,
		},
	})
	var userMsgs, asstMsgs int
	for i := range jres.Events {
		e := jres.Events[i]
		if e.Type == sessionstore.EventSessionStarted {
			continue // a fresh one is injected above
		}
		if beforeSeq > 0 && e.Seq >= beforeSeq {
			continue // point-fork cutoff : drop the fork message and everything after it
		}
		e.Seq = 0 // bus re-assigns
		e.SessionID = newSid
		e.AppID = appID
		e.UserID = userID
		evs = append(evs, e)
		switch e.Type {
		case sessionstore.EventUserMessage:
			userMsgs++
		case sessionstore.EventAssistantMessage:
			asstMsgs++
		}
	}

	ctxApp, cancelApp := appendCtx(r.Context())
	defer cancelApp()
	if _, err := d.sessionStore.AppendDurableBatch(ctxApp, evs); err != nil {
		writeError(w, appendErrStatus(err), "append_failed", err.Error())
		return
	}
	if err := d.sessionStore.SyncMetaToDisk(newSid); err != nil {
		d.logger.Warn("forkSession: sync meta failed", "sid", newSid, "err", err.Error())
	}

	snap, _, _ := sessionstore.ReadSnapshot(d.sessionPaths.SessionDir(srcSid))
	partial := snap != nil && snap.CutoffSeq > 0

	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id":        newSid,
		"source_session_id": srcSid,
		"forked_from":       srcSid,
		"new_session_id":    newSid,
		"forked":            true,
		"title":             title,
		"message_count":     userMsgs + asstMsgs,
		"events_copied":     len(evs) - 1,
		"partial_history":   partial,
		"instance_id":       d.envelopeBuilder.InstanceID,
	})
}

// exportSession returns a portable copy of a session. `format=markdown`
// (default) renders a human-readable transcript ; `format=json` returns the
// structured envelope (full message list + metadata). The transcript is the
// lossless one (snapshot + events), so a compacted session still exports its
// whole history.
func (d *Daemon) exportSession(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "json" {
		writeError(w, http.StatusBadRequest, "bad_format", "unsupported format: use 'markdown' or 'json'")
		return
	}

	transcript, err := d.sessionStore.Transcript(sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}

	state.RLock()
	appID := state.AppID
	title := state.Title
	workspace := state.Workspace
	wd := state.Workdir
	firstSeq := state.FirstSeq
	lastSeq := state.LastSeq
	state.RUnlock()
	if title == "" {
		title = "Untitled Session"
	}

	turns := 0
	for i := range transcript {
		if transcript[i].Role == "user" {
			turns++
		}
	}

	if format == "json" {
		writeJSON(w, http.StatusOK, map[string]any{
			"format":        "json",
			"session_id":    sid,
			"app_id":        appID,
			"title":         title,
			"workspace":     workspace,
			"workdir":       wd,
			"first_seq":     firstSeq,
			"last_seq":      lastSeq,
			"turns":         turns,
			"message_count": len(transcript),
			"messages":      transcript,
			"instance_id":   d.envelopeBuilder.InstanceID,
		})
		return
	}

	md := renderTranscriptMarkdown(title, appID, sid, transcript, turns)
	writeJSON(w, http.StatusOK, map[string]any{
		"content":       md,
		"format":        "markdown",
		"filename":      fmt.Sprintf("%s_%s.md", sanitizeFilename(appID), shortSessionID(sid)),
		"turns":         turns,
		"message_count": len(transcript),
	})
}

// renderTranscriptMarkdown turns a transcript into a readable document : a
// title + metadata header, then one "## Turn N" block per user message with the
// user text, the assistant reply, and a compact note for any tool the assistant
// called. System messages are skipped (steering context, not conversation).
func renderTranscriptMarkdown(title, appID, sid string, transcript []sessionstore.Message, turns int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "**App:** %s | **Session:** `%s`\n\n---\n\n", appID, shortSessionID(sid))

	turn := 0
	for i := range transcript {
		m := &transcript[i]
		content := strings.TrimSpace(m.Content)
		switch m.Role {
		case "user":
			turn++
			fmt.Fprintf(&b, "## Turn %d\n\n**User:**\n\n%s\n\n", turn, content)
		case "assistant":
			if content != "" {
				fmt.Fprintf(&b, "**Assistant:**\n\n%s\n\n", content)
			}
			for _, id := range m.ToolCallIDs {
				if id != "" {
					fmt.Fprintf(&b, "> 🔧 `%s`\n\n", id)
				}
			}
		case "tool":
			if content != "" {
				if len(content) > 500 {
					content = content[:500] + "\n… (truncated)"
				}
				fmt.Fprintf(&b, "> **Result:**\n>\n> %s\n\n", strings.ReplaceAll(content, "\n", "\n> "))
			}
		}
	}
	fmt.Fprintf(&b, "---\n\n*Exported from Digitorn · %d turns · %d messages*\n", turns, len(transcript))
	return b.String()
}

// forkLabel builds the "Fork of …" suffix : the source title when present,
// else a short slice of its id.
func forkLabel(title, sid string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return shortSessionID(sid)
}

// shortSessionID returns the first 8 chars of a session id for display.
func shortSessionID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

// sanitizeFilename keeps a string safe for a download filename.
func sanitizeFilename(s string) string {
	if s == "" {
		return "session"
	}
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	return strings.Map(repl, s)
}
