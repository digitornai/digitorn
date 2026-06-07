package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

// previewKey lazily loads the HMAC key for preview tokens. STABLE across daemon
// restarts: a restart must not 403 the preview URLs already held by open web
// tabs. Persisted next to the other daemon secrets under the sessions root.
func (d *Daemon) previewKey() []byte {
	d.previewSecretOnce.Do(func() {
		d.previewSecret = d.loadOrCreatePreviewSecret()
	})
	return d.previewSecret
}

func randomPreviewKey(d *Daemon) []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return []byte(fmt.Sprintf("digitorn-preview-fallback-%p", d))
	}
	return b
}

// loadOrCreatePreviewSecret returns a 32-byte key persisted at
// <sessions.root>/.preview-secret so it survives restarts. On the first run it
// generates + writes the key (0600); on any path/IO problem it degrades to a
// process-random key (tokens then rotate on restart — the old behaviour).
func (d *Daemon) loadOrCreatePreviewSecret() []byte {
	root := ""
	if d.cfg != nil {
		root = d.cfg.Sessions.Root
	}
	if root == "" {
		return randomPreviewKey(d)
	}
	p := filepath.Join(root, ".preview-secret")
	if data, err := os.ReadFile(p); err == nil {
		if b, derr := hex.DecodeString(strings.TrimSpace(string(data))); derr == nil && len(b) == 32 {
			return b
		}
	}
	b := randomPreviewKey(d)
	if err := os.MkdirAll(root, 0o755); err == nil {
		_ = os.WriteFile(p, []byte(hex.EncodeToString(b)), 0o600)
	}
	return b
}

// previewToken is a stateless, unguessable per-(app,session) token. The iframe
// loads the served URL by direct browser navigation, so it CANNOT carry the JWT —
// this token in the query string is the authorization for the public /serve
// route. HMAC over a restart-stable key (loadOrCreatePreviewSecret): no per-call
// storage, no race, and a daemon restart no longer invalidates live preview URLs.
func (d *Daemon) previewToken(appID, sessionID string) string {
	mac := hmac.New(sha256.New, d.previewKey())
	mac.Write([]byte(appID + "\x00" + sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Daemon) checkPreviewToken(appID, sessionID, tok string) bool {
	if tok == "" {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(d.previewToken(appID, sessionID)))
}

// previewWorkdir resolves a session's workdir WITHOUT the ownership check — the
// preview token is the authorization. Sub-agent sessions resolve to their root.
func (d *Daemon) previewWorkdir(sid string) string {
	lookupID := sid
	if root, _, isSub := sessionstore.SubAgentSession(sid); isSub {
		lookupID = root
	}
	state, err := d.sessionStore.State(lookupID)
	if err != nil {
		return ""
	}
	state.RLock()
	wd := state.Workdir
	state.RUnlock()
	return wd
}

// previewEntryCandidates are the entry HTMLs tried, in order, when the preview
// source is "auto". We never run a build or dev-server — we only serve what the
// agent already built. BUILT output dirs are tried FIRST, the workdir root LAST:
// a Vite/CRA project keeps a SOURCE index.html at the root (it points at
// `/src/main.jsx` / `%PUBLIC_URL%`, which only a dev-server can resolve), so the
// real built entry under dist/build/out must win. The root entry is the fallback
// for a hand-written plain static site with no build step.
var previewEntryCandidates = []string{
	"dist/index.html",
	"build/index.html",
	"out/index.html",
	"public/index.html",
	"index.html",
}

// resolvePreviewEntry returns the first existing, SERVABLE entry HTML
// (workdir-relative, forward-slash) or "" when nothing is built yet. It checks
// the workdir root first, then ONE level of immediate subdirectories — agents
// commonly scaffold the project in a subdir (my-react-app/, frontend/, client/),
// so a build at <wd>/my-react-app/dist/index.html must be found too. A source/dev
// template is not servable (serving it yields a guaranteed blank page — the
// browser asks the daemon for /src/main.jsx and gets a 404), so it is skipped.
func resolvePreviewEntry(wd string) string {
	if e := findPreviewEntryIn(wd, ""); e != "" {
		return e
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		return ""
	}
	for _, de := range entries {
		if !de.IsDir() || skipPreviewSubdir(de.Name()) {
			continue
		}
		if e := findPreviewEntryIn(wd, de.Name()); e != "" {
			return e
		}
	}
	return ""
}

// findPreviewEntryIn checks the candidate entry HTMLs under <wd>/<sub> (sub may
// be ""), skipping un-built source templates. Returns the forward-slash path
// relative to wd, or "".
func findPreviewEntryIn(wd, sub string) string {
	for _, c := range previewEntryCandidates {
		rel := c
		if sub != "" {
			rel = path.Join(sub, c)
		}
		abs := filepath.Join(wd, filepath.FromSlash(rel))
		fi, err := os.Stat(abs)
		if err != nil || fi.IsDir() {
			continue
		}
		if isUnbuiltTemplate(abs) {
			continue
		}
		return rel
	}
	return ""
}

// skipPreviewSubdir excludes dirs that can't hold a project root or are already
// covered by the root scan (dist/build/out/public) — and the big/irrelevant ones.
func skipPreviewSubdir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "dist", "build", "out", "public", "src", "vendor", "target", "coverage":
		return true
	}
	return false
}

// isUnbuiltTemplate reports whether an entry HTML is a dev/source template rather
// than a built artifact. Built output references hashed `/assets/*.js`; a source
// template references the un-bundled dev entry — Vite's `/src/...` or `/@vite`,
// CRA's `%PUBLIC_URL%`. Reads only the head (entry HTMLs are tiny); on any read
// error it errs toward "servable" (returns false) so a real build is never hidden.
func isUnbuiltTemplate(abs string) bool {
	b, err := os.ReadFile(abs)
	if err != nil {
		return false
	}
	s := strings.ToLower(string(b))
	return strings.Contains(s, "/src/") ||
		strings.Contains(s, "%public_url%") ||
		strings.Contains(s, "/@vite") ||
		strings.Contains(s, "/@react-refresh")
}

func (d *Daemon) previewBaseURL() string {
	host, port := "127.0.0.1", 8000
	if d.cfg != nil {
		if d.cfg.Server.Host != "" {
			host = d.cfg.Server.Host
		}
		if d.cfg.Server.Port != 0 {
			port = d.cfg.Server.Port
		}
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// getWebPreview resolves the preview source for (app, session) and returns the
// iframe URL. Today: auto-detect a built entry HTML under the workdir and serve
// it. `attached` is false when nothing is built yet. Authenticated — the web
// client polls it (with the JWT) to learn the iframe URL.
func (d *Daemon) getWebPreview(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	sid := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"attached": false, "url": nil})
		return
	}
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	entry := ""
	if wd != "" {
		entry = resolvePreviewEntry(wd)
	}
	if entry == "" {
		writeJSON(w, http.StatusOK, map[string]any{"attached": false, "url": nil})
		return
	}
	previewURL := fmt.Sprintf("%s/api/apps/%s/sessions/%s/preview/serve/%s?t=%s",
		d.previewBaseURL(), appID, sid, entry, d.previewToken(appID, sid))
	writeJSON(w, http.StatusOK, map[string]any{"attached": true, "url": previewURL})
}

// pushPreviewSource re-resolves the preview source for a session root and PUSHES
// the iframe URL to its realtime room as `web_preview:attached` — so the client
// never has to poll. Called (debounced) on every workspace change. It dedups by
// the entry file's mtime so a no-op write doesn't reload the iframe; a real
// rebuild (new mtime) pushes a fresh payload that forces the reload.
func (d *Daemon) pushPreviewSource(ctx context.Context, root string) {
	if d.rt == nil || root == "" {
		return
	}
	state, err := d.sessionStore.State(root)
	if err != nil {
		return
	}
	state.RLock()
	appID := state.AppID
	wd := state.Workdir
	state.RUnlock()
	if appID == "" || wd == "" {
		return
	}
	entry := resolvePreviewEntry(wd)
	if entry == "" {
		d.previewLastKey.Delete(root) // build gone — next build re-pushes
		return
	}
	fi, err := os.Stat(filepath.Join(wd, filepath.FromSlash(entry)))
	if err != nil {
		return
	}
	key := fmt.Sprintf("%s|%d", entry, fi.ModTime().UnixNano())
	if prev, ok := d.previewLastKey.Load(root); ok && prev.(string) == key {
		return // nothing rebuilt since the last push — don't reload for nothing
	}
	d.previewLastKey.Store(root, key)
	previewURL := fmt.Sprintf("%s/api/apps/%s/sessions/%s/preview/serve/%s?t=%s",
		d.previewBaseURL(), appID, root, entry, d.previewToken(appID, root))
	_ = d.rt.Emit(ctx, bridgeNamespace, "session:"+root, "web_preview:attached", map[string]any{
		"session_id": root,
		"name":       "default",
		"url":        previewURL,
		"type":       "static",
	})
}

// servePreviewFile serves a file from the session workdir for the iframe, with a
// browser Content-Type. PUBLIC route (no JWT) — the `t` token is the auth. Confined
// to the workdir by PathPolicy; the shadow repo (.digitorn) is refused. A directory
// (or trailing slash / empty path) serves its index.html.
func (d *Daemon) servePreviewFile(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	sid := chi.URLParam(r, "session_id")
	if !d.checkPreviewToken(appID, sid, r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	wd := d.previewWorkdir(sid)
	if wd == "" {
		http.Error(w, "no workspace", http.StatusNotFound)
		return
	}
	rel := chi.URLParam(r, "*")
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}
	if isShadowRel(rel) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pp := workdir.NewPolicy(workdir.Options{Root: wd})
	abs, err := pp.Enforce(rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if fi.IsDir() {
		rel = path.Join(rel, "index.html")
		abs = filepath.Join(abs, "index.html")
		if _, serr := os.Stat(abs); serr != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	// Remember which build the iframe is now showing, so its ROOT-absolute assets
	// (default Vite/CRA emit /assets/*) — which hit the daemon origin, not this
	// nested path — resolve via previewRootFallback. The build root is the entry's
	// directory (e.g. "dist"). Only an HTML entry sets it.
	if strings.HasSuffix(strings.ToLower(rel), ".html") {
		buildRoot := path.Dir(rel)
		if buildRoot == "." {
			buildRoot = ""
		}
		d.activePreview.Store(&activePreviewState{workdir: wd, buildRoot: buildRoot})
	}
	d.serveStaticFile(w, r, abs)
}

// activePreviewState is the build the workspace Preview iframe currently shows.
// Its root-absolute assets are served from <workdir>/<buildRoot> by the 404
// fallback. One active preview per daemon — the workspace shows one at a time.
type activePreviewState struct {
	workdir   string
	buildRoot string // forward-slash, relative to workdir ("" = workdir root)
}

// serveStaticFile streams a workdir file to the iframe with a browser
// Content-Type. ServeContent keeps our Content-Type and adds Range support;
// unlike ServeFile it does no index.html redirect. X-Frame-Options is left unset
// on purpose — this content is meant to be framed by the Preview iframe.
func (d *Daemon) serveStaticFile(w http.ResponseWriter, r *http.Request, abs string) {
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", previewContentType(abs))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(abs), fi.ModTime(), f)
}

// previewRootFallback is the router's 404 handler. A default Vite/CRA build loads
// its assets by ROOT-absolute paths (/assets/*) that hit the daemon origin, not
// the nested /preview/serve path, so they would 404 and the page renders blank.
// When an iframe is showing a build (activePreview set), unmatched GETs are served
// from that build's directory — covering the entry's stylesheet/script AND the
// runtime-loaded code-split chunks. Otherwise it is a normal 404. Confined to the
// active workdir by PathPolicy; the shadow repo is refused.
func (d *Daemon) previewRootFallback(w http.ResponseWriter, r *http.Request) {
	ap := d.activePreview.Load()
	if ap == nil || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		rel = "index.html"
	}
	if isShadowRel(rel) {
		http.NotFound(w, r)
		return
	}
	joined := rel
	if ap.buildRoot != "" {
		joined = ap.buildRoot + "/" + rel
	}
	abs, err := workdir.NewPolicy(workdir.Options{Root: ap.workdir}).Enforce(joined)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi, serr := os.Stat(abs); serr != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	d.serveStaticFile(w, r, abs)
}

// previewContentType picks a browser Content-Type for a served file. The web set
// is fixed explicitly (Windows' mime registry mislabels .js / .css), then we fall
// back to the platform table, then octet-stream.
func previewContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs", ".cjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	return "application/octet-stream"
}
