package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/docstore"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
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
	// App-embedded preview takes precedence: if the app ships a built UI at
	// {app}/web/dist, point the iframe there. It's the SAME bundle for every
	// session (not workdir-resolved). The URL carries everything the in-page
	// SDK needs to self-bootstrap — app, session, and the per-session preview
	// token — so it can hit the `?t=`-authed /preview/files routes without a
	// JWT and with zero host wiring. No workdir scan, no per-write reload.
	if _, err := os.Stat(filepath.Join(d.cfg.Apps.Root, appID, "web", "dist", "index.html")); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"attached": true,
			"url": fmt.Sprintf("%s/api/apps/%s/web-static/?app=%s&session=%s&t=%s",
				d.previewBaseURL(), appID,
				url.QueryEscape(appID), url.QueryEscape(sid),
				d.previewToken(appID, sid)),
			"type": "web",
		})
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
	// Embedded-web apps ({app}/web/dist) have a STABLE, shared preview UI — never
	// push a reload on a workspace change: the in-page SDK applies live updates
	// itself. Reloading the iframe on every agent write would wipe its state.
	if _, err := os.Stat(filepath.Join(d.cfg.Apps.Root, appID, "web", "dist", "index.html")); err == nil {
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
	sid := sessionIDParam(r)
	if !d.checkPreviewToken(appID, sid, r.URL.Query().Get("t")) {
		// The iframe's sub-resource requests (CSS/JS/img referenced by the
		// preview HTML) don't carry the `?t=` query — browsers never propagate
		// a query string to sub-resources. Serve static (non-HTML) assets
		// tokenlessly from the currently ACTIVE preview build — exactly the
		// trust model previewRootFallback already uses for root-absolute
		// assets. HTML entries still require the token (they set active state).
		if d.tryServeActivePreviewAsset(w, r) {
			return
		}
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

// tryServeActivePreviewAsset serves a STATIC (non-HTML) asset from the currently
// active preview build without a token — the fallback for the iframe's
// sub-resource requests, which can't carry the `?t=` query. Mirrors
// previewRootFallback's trust model: only the last authed preview's workdir,
// confined by the workdir policy, shadow refused, GET only. Returns false (so
// the caller 403s) for anything it won't serve.
func (d *Daemon) tryServeActivePreviewAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	ap := d.activePreview.Load()
	if ap == nil {
		return false
	}
	rel := chi.URLParam(r, "*")
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	rel = strings.TrimPrefix(rel, "/")
	low := strings.ToLower(rel)
	// Never HTML (entries set the active state — must be token-authed), never a
	// directory, never the internal shadow tree.
	if rel == "" || strings.HasSuffix(rel, "/") ||
		strings.HasSuffix(low, ".html") || strings.HasSuffix(low, ".htm") ||
		isShadowRel(rel) {
		return false
	}
	abs, err := workdir.NewPolicy(workdir.Options{Root: ap.workdir}).Enforce(rel)
	if err != nil {
		return false
	}
	if fi, serr := os.Stat(abs); serr != nil || fi.IsDir() {
		return false
	}
	d.serveStaticFile(w, r, abs)
	return true
}

// previewFilePath authorizes the `?t=` preview token and resolves + confines a
// workdir-relative path for the preview file R/W routes. On failure it writes the
// HTTP error and returns ok=false.
func (d *Daemon) previewFilePath(w http.ResponseWriter, r *http.Request) (abs, wd, sid string, ok bool) {
	appID := chi.URLParam(r, "app_id")
	sid = sessionIDParam(r)
	if !d.checkPreviewToken(appID, sid, r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", "", false
	}
	wd = d.previewWorkdir(sid)
	if wd == "" {
		http.Error(w, "no workspace", http.StatusNotFound)
		return "", "", "", false
	}
	rel := chi.URLParam(r, "*")
	if dec, derr := url.PathUnescape(rel); derr == nil {
		rel = dec
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.HasSuffix(rel, "/") || isShadowRel(rel) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", "", false
	}
	a, err := workdir.NewPolicy(workdir.Options{Root: wd}).Enforce(rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", "", false
	}
	return a, wd, sid, true
}

// getPreviewFile serves the RAW bytes of one workdir file to the embedded preview
// SDK. PUBLIC route — the `?t=` preview token is the auth (the iframe carries it;
// it can't send the JWT). Confined to the session workdir; shadow refused. This
// is the read half the SDK's useFile/useSharedDoc use.
func (d *Daemon) getPreviewFile(w http.ResponseWriter, r *http.Request) {
	abs, _, _, ok := d.previewFilePath(w, r)
	if !ok {
		return
	}
	data, _, err := readFileCapped(abs, workspaceFileMaxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(abs)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// putPreviewFile writes one workdir file from the embedded preview SDK, then fires
// the same FileChanged signal the agent's filesystem writes do — so the workspace
// panel and any other watchers update. PUBLIC route, `?t=`-authed. This is the
// write half of useSharedDoc's two-way sync.
func (d *Daemon) putPreviewFile(w http.ResponseWriter, r *http.Request) {
	abs, wd, sid, ok := d.previewFilePath(w, r)
	if !ok {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := readJSONLenient(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(abs, []byte(body.Content), 0o644); err != nil {
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}
	// Docstore round-trip: the embedded app saving its composed document (the
	// canvas write path) decomposes back onto fragments — diff by id, only the
	// changed fragments rewritten. A fragment written via this route composes.
	switch dir, kind := docstore.FindDocDir(abs); kind {
	case "composed":
		if _, derr := docstore.SyncComposed(abs); derr != nil {
			d.logger.Warn("docstore: decompose failed", "file", abs, "err", derr.Error())
		}
	case "fragment":
		if _, derr := docstore.SyncFragments(dir); derr != nil && !errors.Is(derr, docstore.ErrInvalid) {
			d.logger.Warn("docstore: compose failed", "file", abs, "err", derr.Error())
		}
	}
	if d.workspaceLive != nil {
		// Name the exact file so the change is pushed immediately + reliably
		// (bypassing git-status baseline quirks) to the preview's live socket.
		rel := "" // workdir-relative, slash form
		if r, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = filepath.ToSlash(r)
		}
		d.workspaceLive.FileChanged(sid, wd, rel)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// activePreviewState is the build the workspace Preview iframe currently shows.
// Its root-absolute assets are served from <workdir>/<buildRoot> by the 404
// fallback. One active preview per daemon — the workspace shows one at a time.
type activePreviewState struct {
	workdir   string
	buildRoot string // forward-slash, relative to workdir ("" = workdir root)
}

// previewThemeShim is injected into EVERY preview HTML document the daemon
// serves, so any embedded app — whether or not it uses @digitorn/sdk — adopts
// the host's light/dark theme. It mirrors the SDK's theme handling (contract:
// digitorn_web/src/lib/preview-postmessage.ts) at the platform level: seed from
// `?theme=`, follow `digi:theme-change`, and announce `digi:ready` so the host
// (re)sends the current theme. Runs in <head> before paint to avoid a flash.
const previewThemeShim = `<script>(function(){function r(m){m=(m||"").toLowerCase();if(m==="dark"||m==="light")return m;try{return window.matchMedia&&window.matchMedia("(prefers-color-scheme: dark)").matches?"dark":"light"}catch(e){return "light"}}function a(m,c){var e=document.documentElement;e.setAttribute("data-theme",m);e.style.colorScheme=m;if(c)e.style.setProperty("--digitorn-accent",c)}try{var q=new URLSearchParams(location.search);a(r(q.get("theme")||q.get("mode")),q.get("accent"))}catch(e){}window.addEventListener("message",function(e){var d=e.data;if(!d||d.type!=="digi:theme-change"||!d.theme)return;a(r(d.theme.mode),d.theme.accent)});try{if(window.parent&&window.parent!==window)window.parent.postMessage({type:"digi:ready"},"*")}catch(e){}})();</script>`

// previewErrorShim captures the previewed app's runtime failures (uncaught
// errors, unhandled rejections, console.error) and postMessages them to the host
// as `digi:preview-error`, so a crash surfaces in the Problems panel instead of a
// silent blank page. Passive (listeners + postMessage only, no DOM mutation) so
// it never breaks an embedded web app. Deduped + capped at 50 to avoid flooding.
const previewErrorShim = `<script>(function(){var n=0,seen={};function p(k,m,s,f,l,c){if(n>50)return;var key=k+"|"+m+"|"+(l||0);if(seen[key])return;seen[key]=1;n++;try{if(window.parent&&window.parent!==window)window.parent.postMessage({type:"digi:preview-error",error:{kind:k,message:String(m||""),stack:s?String(s).slice(0,4000):"",source:f||"",line:l||0,column:c||0}},"*")}catch(e){}}window.addEventListener("error",function(e){if(e&&(e.error||e.message))p("error",e.message,e.error&&e.error.stack,e.filename,e.lineno,e.colno)});window.addEventListener("unhandledrejection",function(e){var r=e&&e.reason;p("unhandledrejection",(r&&r.message)||String(r),r&&r.stack,"",0,0)});var ce=console.error;console.error=function(){try{var a=[].slice.call(arguments).map(function(x){return x&&x.message?x.message:String(x)}).join(" ");p("console.error",a,arguments[0]&&arguments[0].stack,"",0,0)}catch(_){}return ce.apply(console,arguments)}})();</script>`

// previewNavShim gives the host real browser-style back/forward over the preview.
// The iframe is cross-origin so the host can't touch its history directly: this
// reports every navigation (push/replace/pop) to the host and executes the host's
// back/forward commands from inside the frame (same-origin to itself). Passive.
const previewNavShim = `<script>(function(){function s(k){try{if(window.parent&&window.parent!==window)window.parent.postMessage({type:"digi:nav",url:location.href,kind:k},"*")}catch(e){}}var p=history.pushState;history.pushState=function(){var r=p.apply(this,arguments);s("push");return r};var rp=history.replaceState;history.replaceState=function(){var r=rp.apply(this,arguments);s("replace");return r};window.addEventListener("popstate",function(){s("pop")});window.addEventListener("hashchange",function(){s("push")});window.addEventListener("message",function(e){var d=e.data;if(!d)return;if(d.type==="digi:nav-back")history.back();else if(d.type==="digi:nav-forward")history.forward()});s("push")})();</script>`

// injectThemeShim inserts the theme + error + nav shims into an HTML document —
// right before </head> when present, else after the opening <head>, else prepended.
func injectThemeShim(html []byte) []byte {
	shim := []byte(previewThemeShim + previewErrorShim + previewNavShim)
	low := bytes.ToLower(html)
	if i := bytes.Index(low, []byte("</head>")); i >= 0 {
		return append(append(append(make([]byte, 0, len(html)+len(shim)), html[:i]...), shim...), html[i:]...)
	}
	if i := bytes.Index(low, []byte("<head")); i >= 0 {
		if j := bytes.IndexByte(html[i:], '>'); j >= 0 {
			pos := i + j + 1
			return append(append(append(make([]byte, 0, len(html)+len(shim)), html[:pos]...), shim...), html[pos:]...)
		}
	}
	return append(append(make([]byte, 0, len(html)+len(shim)), shim...), html...)
}

// serveHTMLWithShim serves an HTML file with the platform theme shim injected.
// Used for every preview HTML entry (workdir builds AND web-static app bundles).
func serveHTMLWithShim(w http.ResponseWriter, abs string) {
	data, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	out := injectThemeShim(data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// isHTMLFile reports whether abs is an HTML document by extension.
func isHTMLFile(abs string) bool {
	e := strings.ToLower(filepath.Ext(abs))
	return e == ".html" || e == ".htm"
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
	// HTML entries get the theme shim so the preview adopts the host theme.
	if isHTMLFile(abs) {
		serveHTMLWithShim(w, abs)
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
