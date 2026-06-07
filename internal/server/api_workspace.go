package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/modules/filesystem"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

// sessionWorkdir returns the on-disk workdir for a session (a sub-agent session
// resolves to its root), enforcing ownership. "" means the session has none.
func (d *Daemon) sessionWorkdir(ctx context.Context, sid string) (string, error) {
	lookupID := sid
	if root, _, isSub := sessionstore.SubAgentSession(sid); isSub {
		lookupID = root
	}
	state, err := d.requireOwnedSession(ctx, lookupID)
	if err != nil {
		return "", err
	}
	state.RLock()
	wd := state.Workdir
	state.RUnlock()
	return wd, nil
}

// invokeWorkspace calls one action on the workspace git module with the session
// workdir injected, returning the result Data. It dispatches through the service
// bus, NOT the in-proc registry, so the call routes transparently to the worker
// pool's ProxyModule when `workspace` is workerised (workers.pools) and to the
// in-proc module otherwise — every git op (REST changes/diff/commit, the
// baseline at session creation, the brique-4 live push) goes off the daemon when
// a pool is configured, with no per-call-site change.
// workspaceCallTimeout is a generous backstop on a single workspace (git) call.
// The pool is count:1 and shared across every session, so one pathological op
// must never hang a request forever or starve the worker for everyone — it fails
// after this and frees the daemon goroutine. Sized so a legitimate large commit
// still completes; it only catches genuine hangs.
const workspaceCallTimeout = 120 * time.Second

func (d *Daemon) invokeWorkspace(ctx context.Context, action, workdir string, extra map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, workspaceCallTimeout)
	defer cancel()
	params := map[string]any{"workdir": workdir}
	for k, v := range extra {
		params[k] = v
	}
	raw, _ := json.Marshal(params)
	res, err := d.bus.Call(ctx, "workspace", action, raw)
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return res.Data, nil
}

// baselineWorkspaceAsync snapshots the workspace's starting state (HEAD
// baseline) in the BACKGROUND, so every later agent change is a clean diff
// against it. Non-blocking: session creation never waits on git.
func (d *Daemon) baselineWorkspaceAsync(workdir string) {
	if workdir == "" {
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				d.logger.Warn("workspace baseline panicked", "workdir", workdir, "panic", rec)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := d.invokeWorkspace(ctx, "baseline", workdir, nil); err != nil {
			d.logger.Warn("workspace baseline failed", "workdir", workdir, "err", err.Error())
		}
	}()
}

// GET …/workspace/changes — the agent's pending file changes since the baseline.
func (d *Daemon) getWorkspaceChanges(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeJSON(w, http.StatusOK, map[string]any{"files": []any{}})
		return
	}
	var extra map[string]any
	switch r.URL.Query().Get("include_diffs") {
	case "1", "true", "yes":
		extra = map[string]any{"include_diffs": true}
	}
	data, err := d.invokeWorkspace(r.Context(), "changes", wd, extra)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// GET …/workspace/diff?path=… — unified diff + numstat for one changed file.
func (d *Daemon) getWorkspaceDiff(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing_path", "query param `path` is required")
		return
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	data, err := d.invokeWorkspace(r.Context(), "diff", wd, map[string]any{"path": path})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// POST …/workspace/commit {message, paths?} — validate (commit) the changes.
func (d *Daemon) postWorkspaceCommit(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	var body struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if r.ContentLength > 0 {
		if err := readJSONLenient(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	data, err := d.invokeWorkspace(r.Context(), "commit", wd, map[string]any{"message": body.Message, "paths": body.Paths})
	if err != nil {
		if strings.Contains(err.Error(), "nothing approved") {
			writeError(w, http.StatusBadRequest, "nothing_to_commit", "approve at least one change before committing")
			return
		}
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	// Committed files leave the pending set — refresh the Changes panel + badges.
	if d.workspaceLive != nil {
		d.workspaceLive.FileChanged(sid, wd)
	}
	writeJSON(w, http.StatusOK, data)
}

// workspaceFileAction backs the approve + reject routes. Both take {path} (one)
// or {paths} (many), CONFINE every path to the session workdir (escape / shadow
// / secret rejected — reject restores files on disk, so this is load-bearing),
// dispatch the matching workspace action, then fire the live poke so the Changes
// panel + gutter badges (approved=green / pending=orange) refresh.
func (d *Daemon) workspaceFileAction(w http.ResponseWriter, r *http.Request, action string) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	var body struct {
		Path    string   `json:"path"`
		Paths   []string `json:"paths"`
		Message string   `json:"message"`
	}
	if err := readJSONLenient(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	paths := append([]string{}, body.Paths...)
	if strings.TrimSpace(body.Path) != "" {
		paths = append(paths, body.Path)
	}
	if len(paths) == 0 {
		writeError(w, http.StatusBadRequest, "missing_path", "provide `path` or `paths`")
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		if isShadowRel(p) {
			writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
			return
		}
		if _, perr := pp.Enforce(p); perr != nil {
			writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
			return
		}
		clean = append(clean, filepath.ToSlash(p))
	}
	data, err := d.invokeWorkspace(r.Context(), action, wd, map[string]any{"paths": clean, "message": body.Message})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if d.workspaceLive != nil {
		d.workspaceLive.FileChanged(sid, wd)
	}
	writeJSON(w, http.StatusOK, data)
}

// POST …/workspace/files/approve {path|paths,message} — approve (commit) the changes.
func (d *Daemon) postWorkspaceApprove(w http.ResponseWriter, r *http.Request) {
	d.workspaceFileAction(w, r, "approve")
}

// POST …/workspace/files/reject {path|paths} — revert the changes to baseline.
func (d *Daemon) postWorkspaceReject(w http.ResponseWriter, r *http.Request) {
	d.workspaceFileAction(w, r, "reject")
}

// POST …/workspace/files/approve-hunks {path,hunks[,message]} — commit only the
// selected hunks of one file.
func (d *Daemon) postWorkspaceApproveHunks(w http.ResponseWriter, r *http.Request) {
	d.workspaceHunkAction(w, r, "approve_hunks")
}

// POST …/workspace/files/reject-hunks {path,hunks} — revert only the selected
// hunks of one file.
func (d *Daemon) postWorkspaceRejectHunks(w http.ResponseWriter, r *http.Request) {
	d.workspaceHunkAction(w, r, "reject_hunks")
}

// workspaceHunkAction is the shared handler for the per-hunk approve/reject
// routes. Body: {path, hunks:[hash...], message?}. It confines the single path
// to the workdir, dispatches the workspace action, then fires the live poke so
// the Changes panel + diff refresh.
func (d *Daemon) workspaceHunkAction(w http.ResponseWriter, r *http.Request, action string) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	var body struct {
		Path    string   `json:"path"`
		Hunks   []string `json:"hunks"`
		Message string   `json:"message"`
	}
	if err := readJSONLenient(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		writeError(w, http.StatusBadRequest, "missing_path", "provide `path`")
		return
	}
	if len(body.Hunks) == 0 {
		writeError(w, http.StatusBadRequest, "missing_hunks", "provide `hunks`")
		return
	}
	if isShadowRel(body.Path) {
		writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	if _, perr := pp.Enforce(body.Path); perr != nil {
		writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
		return
	}
	data, err := d.invokeWorkspace(r.Context(), action, wd, map[string]any{
		"path":    filepath.ToSlash(body.Path),
		"hunks":   body.Hunks,
		"message": body.Message,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if d.workspaceLive != nil {
		d.workspaceLive.FileChanged(sid, wd)
	}
	writeJSON(w, http.StatusOK, data)
}

// POST …/workspace/files/approve-all — approve EVERY pending change ("l'ensemble").
// Backed by the shadow repo's StageAll, which reads the live working-tree status,
// so it stages whatever is pending at click time — correct even if a file
// appeared since the client last fetched /changes (no stale path list, no TOCTOU).
// The .digitorn metadata and any user .git stay excluded, as at baseline.
func (d *Daemon) postWorkspaceApproveAll(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if r.ContentLength > 0 {
		if err := readJSONLenient(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	// Empty paths is the module's "approve all" signal → stage every pending
	// change, then commit them as one revision labelled by the message.
	data, err := d.invokeWorkspace(r.Context(), "approve", wd, map[string]any{"paths": []string{}, "message": body.Message})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if d.workspaceLive != nil {
		d.workspaceLive.FileChanged(sid, wd)
	}
	writeJSON(w, http.StatusOK, data)
}

// workspaceFileMaxBytes caps how much of a file the editor route serves. Bigger
// files are clipped (truncated flag set) — Monaco renders the head, the agent
// still wrote the whole file on disk. Vision-sized, since this is editor text.
const workspaceFileMaxBytes = 5 << 20

// GET …/workspace/files/{path}?include_baseline=… — one file's RAW content for
// the Monaco editor (NOT the agent-formatted `filesystem.read`: no line numbers,
// no clipping notes). ISOLATION: the path is confined to the session workdir by
// the same PathPolicy the agent runs under (workdir.NewPolicy → Enforce), so a
// `..` escape, an absolute path outside the workdir, a planted symlink, or a
// daemon-secret path is rejected — a session can only read its own files.
func (d *Daemon) getWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	rel := chi.URLParam(r, "*")
	// The web client sends the path via encodeURIComponent, so a nested file
	// arrives percent-encoded (frontend%2Fpackage.json) and chi does NOT decode
	// the embedded %2F. Decode it here so "a%2Fb" becomes "a/b" — both the
	// nested-file read AND the `..`-escape rejection depend on the real path.
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	if strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "missing_path", "file path is required")
		return
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	// The per-workdir shadow repo (.digitorn/…) is daemon-internal git
	// plumbing — never served to the editor, matching the tree route which
	// hides it.
	if isShadowRel(rel) {
		writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	abs, err := pp.Enforce(rel)
	if err != nil {
		// Escape / secret / empty — never leak why beyond "forbidden".
		writeError(w, http.StatusForbidden, "forbidden_path", err.Error())
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "no such file under the workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if fi.IsDir() {
		writeError(w, http.StatusBadRequest, "is_dir", "path is a directory")
		return
	}

	raw, truncated, err := readFileCapped(abs, workspaceFileMaxBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	payload := map[string]any{"language": guessLanguage(rel)}
	if isBinaryContent(raw) {
		payload["content"] = ""
		payload["binary"] = true
	} else {
		payload["content"] = string(raw)
	}
	out := map[string]any{
		"path":      filepath.ToSlash(rel),
		"payload":   payload,
		"truncated": truncated,
	}
	// Diff vs the shadow-repo baseline so the editor's Diff overlay has a body
	// without a second round-trip. Best-effort: a diff failure must not block the
	// content (an untracked/binary file may legitimately have none).
	switch r.URL.Query().Get("include_baseline") {
	case "1", "true", "yes":
		if dd, derr := d.invokeWorkspace(r.Context(), "diff", wd, map[string]any{"path": filepath.ToSlash(rel)}); derr == nil {
			if m, ok := dd.(map[string]any); ok {
				if u, ok := m["unified"].(string); ok && u != "" {
					out["unified_diff_pending"] = u
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// workspaceSearchMaxHits caps the hits returned for one search so a broad query
// on a huge tree can never produce an unbounded payload.
const workspaceSearchMaxHits = 2000

// GET …/workspace/search?q=&case=&word=&regex= — search across every file in the
// session workdir (not just open buffers). Backed by the filesystem module's
// trigram grep, confined to the workdir by the session PathPolicy; gitignored
// noise (node_modules, …) is skipped by grep. The match column/length are
// computed server-side from the authoritative Go regexp.
func (d *Daemon) getWorkspaceSearch(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}, "truncated": false})
		return
	}
	flag := func(name string) bool {
		switch r.URL.Query().Get(name) {
		case "1", "true", "yes":
			return true
		}
		return false
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}, "truncated": false})
		return
	}

	// Build the pattern from the literal/regex + word + case options.
	body := q
	if !flag("regex") {
		body = regexp.QuoteMeta(q)
	}
	if flag("word") {
		body = `\b` + body + `\b`
	}
	if !flag("case") {
		body = "(?i)" + body
	}
	re, cerr := regexp.Compile(body)
	if cerr != nil {
		// Invalid regex is a user input error, not a 500 — surface it to the panel.
		writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}, "truncated": false, "regex_error": cerr.Error()})
		return
	}

	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	ctx := workdir.WithPathPolicy(r.Context(), pp)
	ctx = tool.WithIdentity(ctx, tool.Identity{
		SessionID: sid,
		AppID:     chi.URLParam(r, "app_id"),
		UserID:    userIDOf(r.Context()),
	})
	args, _ := json.Marshal(map[string]any{
		"pattern": body, "path": ".", "output_mode": "content", "max_results": workspaceSearchMaxHits,
	})
	res, err := d.bus.Call(ctx, "filesystem", "grep", args)
	if err != nil || !res.Success {
		msg := "search failed"
		if err != nil {
			msg = err.Error()
		} else if res.Error != "" {
			msg = res.Error
		}
		writeError(w, http.StatusInternalServerError, "workspace_error", msg)
		return
	}

	// Parse grep's matches robustly (JSON round-trip works in-proc and worker-side).
	rawData, _ := json.Marshal(res.Data)
	var gd struct {
		Matches []struct {
			File string `json:"file"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	_ = json.Unmarshal(rawData, &gd)

	type hit struct {
		Path        string `json:"path"`
		Filename    string `json:"filename"`
		Line        int    `json:"line"`
		Column      int    `json:"column"`       // 1-based, in characters
		MatchLength int    `json:"match_length"` // in characters
		LineContent string `json:"line_content"`
	}
	hits := make([]hit, 0, len(gd.Matches))
	truncated := gd.Truncated
	for _, m := range gd.Matches {
		fname := filepath.ToSlash(m.File)
		if i := strings.LastIndex(fname, "/"); i >= 0 {
			fname = fname[i+1:]
		}
		for _, loc := range re.FindAllStringIndex(m.Text, -1) {
			if loc[1] == loc[0] {
				continue // zero-length match (e.g. a* ) — skip
			}
			hits = append(hits, hit{
				Path:        filepath.ToSlash(m.File),
				Filename:    fname,
				Line:        m.Line,
				Column:      utf8.RuneCountInString(m.Text[:loc[0]]) + 1,
				MatchLength: utf8.RuneCountInString(m.Text[loc[0]:loc[1]]),
				LineContent: m.Text,
			})
			if len(hits) >= workspaceSearchMaxHits {
				truncated = true
				break
			}
		}
		if len(hits) >= workspaceSearchMaxHits {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "truncated": truncated})
}

// GET …/workspace/files/{path}/history — the file's committed revisions (the
// "Approval history" tab). The path arrives as ONE percent-encoded segment
// (encodeURIComponent). Confined to the session workdir by the same PathPolicy
// as the read route; a deleted file still has history (Enforce checks the path,
// not existence).
func (d *Daemon) getWorkspaceFileHistory(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	rel := chi.URLParam(r, "filepath")
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	if strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "missing_path", "file path is required")
		return
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeJSON(w, http.StatusOK, map[string]any{"revisions": []any{}})
		return
	}
	if isShadowRel(rel) {
		writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	if _, perr := pp.Enforce(rel); perr != nil {
		writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
		return
	}
	data, err := d.invokeWorkspace(r.Context(), "history", wd, map[string]any{"path": filepath.ToSlash(rel)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// POST …/workspace/files/{path}/revert {revision} — restore the file to a past
// revision as a PENDING change (history untouched; the user reviews then approves
// or rejects). Confined to the session workdir; the shadow repo is refused.
func (d *Daemon) postWorkspaceFileRevert(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	rel := chi.URLParam(r, "filepath")
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	if strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "missing_path", "file path is required")
		return
	}
	var body struct {
		Revision int `json:"revision"`
	}
	if err := readJSONLenient(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if body.Revision < 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "revision must be >= 1")
		return
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	if isShadowRel(rel) {
		writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	if _, perr := pp.Enforce(rel); perr != nil {
		writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
		return
	}
	data, err := d.invokeWorkspace(r.Context(), "revert", wd, map[string]any{"path": filepath.ToSlash(rel), "revision": body.Revision})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if d.workspaceLive != nil {
		d.workspaceLive.FileChanged(sid, wd)
	}
	writeJSON(w, http.StatusOK, data)
}

// GET …/workspace/history — the whole workspace history (every approval), newest
// first, each commit carrying the files it changed.
func (d *Daemon) getWorkspaceHistory(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeJSON(w, http.StatusOK, map[string]any{"commits": []any{}})
		return
	}
	data, err := d.invokeWorkspace(r.Context(), "log", wd, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// POST …/workspace/revert {sha, paths?} — restore files to their content at a
// past commit (all the commit's files, or the chosen subset) as a PENDING change
// the user reviews. Every path is confined to the session workdir.
func (d *Daemon) postWorkspaceRevert(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	var body struct {
		Sha   string   `json:"sha"`
		Paths []string `json:"paths"`
	}
	if err := readJSONLenient(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(body.Sha) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "sha is required")
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	clean := make([]string, 0, len(body.Paths))
	for _, p := range body.Paths {
		if isShadowRel(p) {
			writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
			return
		}
		if _, perr := pp.Enforce(p); perr != nil {
			writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
			return
		}
		clean = append(clean, filepath.ToSlash(p))
	}
	data, err := d.invokeWorkspace(r.Context(), "revert_commit", wd, map[string]any{"sha": body.Sha, "paths": clean})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if d.workspaceLive != nil {
		d.workspaceLive.FileChanged(sid, wd)
	}
	writeJSON(w, http.StatusOK, data)
}

// PUT …/workspace/files/{path} {content} — save a file edited in the Monaco
// editor. Routed through `filesystem.write` with the session PathPolicy +
// identity + change-notifier on the context, so it is byte-for-byte the agent's
// own write path: ATOMIC (temp+rename, crash-safe), CONFINED (escape/secret
// rejected), the trigram index refreshed, and the live `workspace_changes` poke
// fired so the Changes panel + gutter badges update. The shadow repo (.digitorn)
// is refused.
func (d *Daemon) putWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	rel := chi.URLParam(r, "*")
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	if strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "missing_path", "file path is required")
		return
	}
	if isShadowRel(rel) {
		writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
		return
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := readJSONLenient(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	if _, perr := pp.Enforce(rel); perr != nil {
		writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
		return
	}
	// Same context the agent's write runs under → same confinement + atomic
	// write + index refresh + live poke. The notifier folds sub-agent sessions to
	// the root room; we only have the user session here, which is exactly right.
	ctx := workdir.WithPathPolicy(r.Context(), pp)
	ctx = tool.WithIdentity(ctx, tool.Identity{
		SessionID: sid,
		AppID:     chi.URLParam(r, "app_id"),
		UserID:    userIDOf(r.Context()),
	})
	if d.workspaceLive != nil {
		ctx = tool.WithFileChangeNotifier(ctx, d.workspaceLive)
	}
	args, _ := json.Marshal(map[string]any{"path": filepath.ToSlash(rel), "content": body.Content})
	res, err := d.bus.Call(ctx, "filesystem", "write", args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if !res.Success {
		writeError(w, http.StatusBadRequest, "write_failed", res.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "bytes": len(body.Content)})
}

// workspaceTreeMaxPerLevel caps how many entries a single directory level
// returns — the floor that keeps a pathological directory (a flat folder with
// 100k files) from producing one giant payload. The client lazy-loads each
// level, so this is per-directory, not per-repo.
const workspaceTreeMaxPerLevel = 5000

// GET …/workspace/tree?path=<dir> — ONE level of the session workdir's file
// tree (the immediate children of <dir>, root by default). The Monaco explorer
// loads the root level on open and fetches deeper levels only as the user
// expands folders, so a 100k-file repo never ships in one shot — this is the
// load-bearing piece for cloud scale. ISOLATION: <dir> is confined to the
// workdir by the same PathPolicy the agent runs under (escape / shadow / secret
// rejected); VCS/build/dependency noise (skipDirs, incl. .digitorn) is dropped.
func (d *Daemon) getWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeJSON(w, http.StatusOK, map[string]any{"path": "", "entries": []any{}, "truncated": false})
		return
	}

	// Resolve the directory to list. Empty / "." = the workdir root.
	abs := wd
	relSlash := ""
	if rel != "" && rel != "." {
		if isShadowRel(rel) {
			writeError(w, http.StatusForbidden, "forbidden_path", "path is daemon-internal")
			return
		}
		pp := workdir.NewPolicy(workdir.Options{Root: wd})
		resolved, perr := pp.Enforce(rel)
		if perr != nil {
			writeError(w, http.StatusForbidden, "forbidden_path", perr.Error())
			return
		}
		abs = resolved
		relSlash = filepath.ToSlash(rel)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "no such directory under the workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if !fi.IsDir() {
		writeError(w, http.StatusBadRequest, "not_dir", "path is not a directory")
		return
	}

	ents, err := os.ReadDir(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}

	type treeEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"` // "file" | "dir"
	}
	entries := make([]treeEntry, 0, len(ents))
	truncated := false
	for _, e := range ents {
		name := e.Name()
		isDir := e.IsDir()
		// VCS/build/dependency noise (and the shadow repo) is pruned at the
		// directory level — never descended into, never surfaced.
		if isDir && filesystem.IsNoiseDir(name) {
			continue
		}
		childRel := name
		if relSlash != "" {
			childRel = relSlash + "/" + name
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		entries = append(entries, treeEntry{Name: name, Path: childRel, Type: typ})
		if len(entries) >= workspaceTreeMaxPerLevel {
			truncated = true
			break
		}
	}
	// Directories first, then files; case-insensitive alpha within each — the
	// order an explorer expects, so the client renders without re-sorting.
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "dir", entries[j].Type == "dir"
		if di != dj {
			return di
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	writeJSON(w, http.StatusOK, map[string]any{"path": relSlash, "entries": entries, "truncated": truncated})
}

// readFileCapped reads up to cap bytes; truncated reports the file was larger.
func readFileCapped(abs string, cap int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, cap+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, err
	}
	if int64(n) > cap {
		return buf[:cap], true, nil
	}
	return buf[:n], false, nil
}

// isShadowRel reports whether a workdir-relative path points into the daemon's
// per-workdir shadow repo (<workdir>/.digitorn/…) — internal git plumbing that
// is never exposed to the editor or the file tree.
func isShadowRel(rel string) bool {
	s := filepath.ToSlash(strings.TrimPrefix(rel, "./"))
	return s == ".digitorn" || strings.HasPrefix(s, ".digitorn/")
}

// isBinaryContent flags content the editor should not render as text: a NUL byte
// in the first 8 KiB is the universal binary signal.
func isBinaryContent(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// guessLanguage maps a file extension to a Monaco language id. "" lets Monaco
// auto-detect. Kept in sync with the web client's guessLanguage.
func guessLanguage(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	switch ext {
	case "ts", "tsx":
		return "typescript"
	case "js", "jsx", "mjs", "cjs":
		return "javascript"
	case "py":
		return "python"
	case "go":
		return "go"
	case "rs":
		return "rust"
	case "json":
		return "json"
	case "yaml", "yml":
		return "yaml"
	case "md", "mdx":
		return "markdown"
	case "html", "htm":
		return "html"
	case "css":
		return "css"
	case "scss":
		return "scss"
	case "sh", "bash":
		return "shell"
	case "java":
		return "java"
	case "c", "h":
		return "c"
	case "cpp", "hpp", "cc":
		return "cpp"
	case "sql":
		return "sql"
	case "toml":
		return "toml"
	case "xml":
		return "xml"
	default:
		return ""
	}
}
