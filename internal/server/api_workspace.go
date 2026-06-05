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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
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
func (d *Daemon) invokeWorkspace(ctx context.Context, action, workdir string, extra map[string]any) (any, error) {
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
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
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

// GET …/workspace/tree — the session workdir's file list (paths relative to the
// workdir), so the Monaco explorer can render the folder structure including
// files the agent did NOT change (an existing project). ISOLATION: dispatched
// through `filesystem.glob` with the session PathPolicy on the context, so the
// walk is confined to the workdir (symlink escapes dropped) and VCS/build noise
// + .gitignore are pruned — the exact confinement the agent's own glob uses.
func (d *Daemon) getWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeJSON(w, http.StatusOK, map[string]any{"files": []any{}, "count": 0, "truncated": false})
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	ctx := workdir.WithPathPolicy(r.Context(), pp)
	args, _ := json.Marshal(map[string]any{"pattern": "**", "type": "file", "max_results": 10000})
	res, err := d.bus.Call(ctx, "filesystem", "glob", args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
		return
	}
	if !res.Success {
		writeError(w, http.StatusInternalServerError, "workspace_error", res.Error)
		return
	}
	var parsed struct {
		Files     []string `json:"files"`
		Truncated bool     `json:"truncated"`
	}
	rawData, _ := json.Marshal(res.Data)
	_ = json.Unmarshal(rawData, &parsed)
	// Drop the daemon's per-workdir shadow repo (<workdir>/.digitorn/...): it
	// tracks the agent's changes and is invisible to the user's file tree.
	files := make([]string, 0, len(parsed.Files))
	for _, f := range parsed.Files {
		if isShadowRel(f) {
			continue
		}
		files = append(files, filepath.ToSlash(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "count": len(files), "truncated": parsed.Truncated})
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
