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

var previewEntryCandidates = []string{
	"dist/index.html",
	"build/index.html",
	"out/index.html",
	"public/index.html",
	"index.html",
}

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

// previewAgentShim is the agent's eyes and hands on the running app.
//
// The page is served BY the daemon, so it is same-origin with it: this reports
// what the app currently is (rendered or blank, route, visible text, actionable
// elements, runtime failures) and picks up the commands the agent queued, over
// one plain POST. No socket, no relay through the web client, nothing added to a
// path that already works.
//
// It self-disables unless its own URL is a session-scoped preview carrying a
// `?t=` token, so the shared app bundles served from /web-static — which belong
// to no session — never report anything and never receive a command.
//
// Wrapped whole in try/catch: this is injected into every preview, and a broken
// pair of eyes must never break the app the user is looking at.
const previewAgentShim = `<script>(function(){try{var m=location.pathname.match(/^\/api\/apps\/([^\/]+)\/sessions\/([^\/]+)\/preview\/serve\//);if(!m)return;var tok=new URLSearchParams(location.search).get("t");if(!tok)return;var base="/api/apps/"+m[1]+"/sessions/"+m[2]+"/preview/runtime?t="+encodeURIComponent(tok);var errs=[],refs={},busy=false,fresh=true;function push(k,msg,st,f,l,c){if(errs.length>25)return;errs.push({kind:k,message:String(msg||"").slice(0,2000),stack:st?String(st).slice(0,4000):"",source:f||"",line:l||0,column:c||0});}addEventListener("error",function(e){if(e&&(e.error||e.message)){push("error",e.message,e.error&&e.error.stack,e.filename,e.lineno,e.colno);soon();}});addEventListener("unhandledrejection",function(e){var r=e&&e.reason;push("unhandledrejection",(r&&r.message)||String(r),r&&r.stack,"",0,0);soon();});var ce=console.error;console.error=function(){try{var a=[].slice.call(arguments).map(function(x){return x&&x.message?x.message:String(x)}).join(" ");push("console.error",a,arguments[0]&&arguments[0].stack,"",0,0);soon();}catch(_){}return ce.apply(console,arguments)};var reqs=[],logs=[];function fail(mth,u,st,er){if(reqs.length>25)return;reqs.push({method:String(mth||"GET").toUpperCase(),url:String(u||"").slice(0,500),status:st||0,error:er?String(er).slice(0,300):""});soon()}try{var of=window.fetch;if(of)window.fetch=function(i,o){var u=(i&&i.url)||i,m=(o&&o.method)||(i&&i.method)||"GET";return of.apply(this,arguments).then(function(r){if(r&&!r.ok)fail(m,u,r.status,"");return r},function(e){fail(m,u,0,(e&&e.message)||String(e));throw e})}}catch(e){}try{var oo=XMLHttpRequest.prototype.open,os=XMLHttpRequest.prototype.send;XMLHttpRequest.prototype.open=function(m,u){this.__dm=m;this.__du=u;return oo.apply(this,arguments)};XMLHttpRequest.prototype.send=function(){var x=this;x.addEventListener("load",function(){if(x.status>=400)fail(x.__dm,x.__du,x.status,"")});x.addEventListener("error",function(){fail(x.__dm,x.__du,0,"network error")});return os.apply(this,arguments)}}catch(e){}function cap(lv){var o=console[lv];if(!o)return;console[lv]=function(){try{if(logs.length<40){var a=[].slice.call(arguments).map(function(x){try{return x&&x.message?x.message:(typeof x==="object"?JSON.stringify(x):String(x))}catch(e){return String(x)}}).join(" ");logs.push({level:lv,text:a.slice(0,500)})}}catch(e){}return o.apply(console,arguments)}}cap("log");cap("warn");cap("info");function vis(el){try{var r=el.getBoundingClientRect();return !!(r.width||r.height)}catch(e){return false}}function lab(el){return String(el.getAttribute("aria-label")||el.placeholder||el.textContent||"").replace(/\s+/g," ").trim().slice(0,200)}function roleOf(el){var t=el.tagName.toLowerCase();return /^h[1-6]$/.test(t)?"heading":(t==="a"?"link":((t==="input"||t==="textarea"||t==="select")?"field":"button"))}var lastSeen={};function scan(){refs={};var out=[],n=0;var q=document.querySelectorAll("h1,h2,h3,h4,h5,h6,a[href],button,input,textarea,select,[role=button],[role=link]");for(var i=0;i<q.length&&out.length<300;i++){var el=q[i];if(!vis(el))continue;var t=el.tagName.toLowerCase();var ref="e"+(++n);refs[ref]=el;var role=roleOf(el);lastSeen[ref]=lab(el);lastSeen[ref+":role"]=role;out.push({ref:ref,role:role,text:lab(el),level:/^h[1-6]$/.test(t)?parseInt(t.charAt(1),10):0,name:el.name||el.id||"",value:el.value!==undefined?String(el.value).slice(0,200):"",href:el.href||""})}return out}function txt(){try{var b=document.body;if(!b)return "";var t=b.innerText;if(t===undefined||t===null||t==="")t=b.textContent||"";return String(t).replace(/[ \t]+/g," ").replace(/\n{3,}/g,"\n\n").trim().slice(0,20000)}catch(e){return ""}}var cvx=null;function toRGB(c){try{if(!cvx){var cv=document.createElement("canvas");cv.width=1;cv.height=1;cvx=cv.getContext("2d",{willReadFrequently:true})}if(!cvx)return null;cvx.clearRect(0,0,1,1);cvx.fillStyle="#000000";cvx.fillStyle=c;cvx.fillRect(0,0,1,1);var d=cvx.getImageData(0,0,1,1).data;if(d[3]===0)return null;return [d[0],d[1],d[2]]}catch(e){return null}}function lum(c){var v=toRGB(c);if(!v)return null;var a=v.map(function(x){x=x/255;return x<=0.03928?x/12.92:Math.pow((x+0.055)/1.055,2.4)});return 0.2126*a[0]+0.7152*a[1]+0.0722*a[2]}function bg(el){var n=el;while(n&&n!==document.documentElement){var c=getComputedStyle(n).backgroundColor;if(c&&c.indexOf("rgba(0, 0, 0, 0)")<0&&c!=="transparent")return c;n=n.parentElement}return "rgb(255,255,255)"}function audit(){try{var de=document.documentElement,vw=de.clientWidth||0;var ox=Math.max(0,Math.round((Math.max(de.scrollWidth,document.body?document.body.scrollWidth:0))-vw));var tiny=0,low=0,samples=[];var q=document.querySelectorAll("p,span,a,li,button,h1,h2,h3,h4,h5,h6,label,td,div");for(var i=0;i<q.length&&i<600;i++){var el=q[i];if(!el.firstChild||el.firstChild.nodeType!==3)continue;var t=(el.textContent||"").trim();if(!t)continue;var r=el.getBoundingClientRect();if(!(r.width||r.height))continue;var st=getComputedStyle(el),fs=parseFloat(st.fontSize)||16;if(fs<12){tiny++;if(samples.length<8)samples.push(Math.round(fs)+"px: "+t.slice(0,40))}var l1=lum(st.color),l2=lum(bg(el));if(l1!==null&&l2!==null){var hi=Math.max(l1,l2),lo=Math.min(l1,l2),ratio=(hi+0.05)/(lo+0.05);var need=fs>=18?3:4.5;if(ratio<need){low++;if(samples.length<8)samples.push("contraste "+ratio.toFixed(1)+": "+t.slice(0,40))}}}return{overflow_x:ox,tiny_text:tiny,low_contrast:low,samples:samples}}catch(e){return null}}function store(){try{var o={},n=0;for(var i=0;i<localStorage.length&&n<20;i++){var k=localStorage.key(i);o[k]=String(localStorage.getItem(k)||"").slice(0,300);n++}return o}catch(e){return null}}var pendingDetail=null;function detail(ref){try{var el=refs[ref];if(!el)return null;var r=el.getBoundingClientRect(),st=getComputedStyle(el);var at={};for(var i=0;i<el.attributes.length&&i<20;i++){at[el.attributes[i].name]=String(el.attributes[i].value).slice(0,300)}var keys=["display","visibility","opacity","pointer-events","position","z-index","cursor","color","background-color","font-size","overflow"];var sy={};for(var j=0;j<keys.length;j++){sy[keys[j]]=st.getPropertyValue(keys[j])}var cov=false;try{var cx=r.left+r.width/2,cy=r.top+r.height/2;var top=document.elementFromPoint(cx,cy);cov=!!(top&&top!==el&&!el.contains(top)&&!(top.closest&&top.closest("[data-digitorn]")))}catch(e){}return{ref:ref,tag:el.tagName.toLowerCase(),attrs:at,styles:sy,rect:Math.round(r.width)+"x"+Math.round(r.height)+" @ "+Math.round(r.left)+","+Math.round(r.top),covered:cov,disabled:!!el.disabled,html:String(el.outerHTML||"").slice(0,2000)}}catch(e){return null}}function vp(){var w=window.innerWidth||0;return w<=480?"mobile":(w<=1024?"tablet":"desktop")}function snap(){var els=scan(),t=txt();return{url:location.href,title:document.title||"",fresh:fresh,ready:document.readyState!=="loading",blank:t.length===0&&els.length===0,text:t,elements:els,viewport:vp(),errors:errs.splice(0,errs.length),failed_requests:reqs.splice(0,reqs.length),logs:logs.splice(0,logs.length),layout:audit(),storage:store(),detail:pendingDetail?(function(){var d=detail(pendingDetail);pendingDetail=null;return d})():null}}function post(f,s){return fetch(base,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({"for":f,snapshot:s})}).then(function(r){return r.ok?r.json():null})}function setVal(el,v){try{var proto=el.tagName==="TEXTAREA"?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;var d=Object.getOwnPropertyDescriptor(proto,"value");if(d&&d.set){d.set.call(el,v)}else{el.value=v}el.dispatchEvent(new Event("input",{bubbles:true}));el.dispatchEvent(new Event("change",{bubbles:true}))}catch(e){el.value=v}}var ov=null,ring=null,hideT=0;function ui(){if(ov)return;try{ov=document.createElement("div");ov.setAttribute("data-digitorn","");ov.style.cssText="position:fixed;inset:0;pointer-events:none;z-index:2147483647";var st=document.createElement("style");st.textContent="@keyframes dgtp{0%,100%{opacity:1}50%{opacity:.3}}";var pill=document.createElement("div");pill.style.cssText="position:absolute;top:12px;left:50%;transform:translateX(-50%);display:flex;align-items:center;gap:7px;padding:6px 13px;border-radius:999px;background:rgba(15,15,17,.88);color:#fff;font:500 12px/1 system-ui,-apple-system,sans-serif;box-shadow:0 8px 28px rgba(0,0,0,.4)";var dot=document.createElement("span");dot.style.cssText="width:7px;height:7px;border-radius:50%;background:#14B8A6;animation:dgtp 1s ease-in-out infinite";pill.appendChild(dot);pill.appendChild(document.createTextNode("Digitorn"));ring=document.createElement("div");ring.style.cssText="position:absolute;border:2px solid #14B8A6;border-radius:8px;box-shadow:0 0 0 4px rgba(20,184,166,.22);transition:all .18s cubic-bezier(.4,0,.2,1);opacity:0";ov.appendChild(st);ov.appendChild(pill);ov.appendChild(ring);document.documentElement.appendChild(ov)}catch(e){}}function mark(el){try{ui();if(hideT){clearTimeout(hideT);hideT=0}if(!ring)return;if(!el){ring.style.opacity="0";return}var r=el.getBoundingClientRect();ring.style.opacity="1";ring.style.left=(r.left-3)+"px";ring.style.top=(r.top-3)+"px";ring.style.width=(r.width+6)+"px";ring.style.height=(r.height+6)+"px"}catch(e){}}function done(){try{if(hideT)clearTimeout(hideT);hideT=setTimeout(function(){try{if(ov&&ov.parentNode)ov.parentNode.removeChild(ov)}catch(e){}ov=null;ring=null;hideT=0},900)}catch(e){}}function waitFor(needle,limit,id){(function look(){var found=(txt().indexOf(needle)>=0)||scan().some(function(e){return (e.text||"").indexOf(needle)>=0});if(found||Date.now()>limit){if(!found)push("error","wait_for timed out on "+needle,"","",0,0);post(id,snap())}else{setTimeout(look,250)}})()}function norm(x){return String(x||"").replace(/\s+/g," ").trim().toLowerCase()}function find(c){if(c.ref&&refs[c.ref]&&document.contains(refs[c.ref]))return refs[c.ref];var want=norm(c.text_match||c.name||"");if(!want&&c.ref&&lastSeen[c.ref])want=norm(lastSeen[c.ref]);if(!want)return null;var role=c.role||(lastSeen[c.ref+":role"]||"");var q=document.querySelectorAll("h1,h2,h3,h4,h5,h6,a[href],button,input,textarea,select,[role=button],[role=link]");var exact=null,partial=null;for(var i=0;i<q.length;i++){var el=q[i];if(!vis(el))continue;if(role&&roleOf(el)!==role)continue;var t=norm(lab(el));if(!t)continue;if(t===want){exact=el;break}if(!partial&&t.indexOf(want)>=0)partial=el}return exact||partial}function reach(el){try{var r=el.getBoundingClientRect();var h=window.innerHeight||0;if(r.bottom<0||r.top>h){el.scrollIntoView({block:"center"});}}catch(e){}}function reachable(el){if(el.disabled)return "the element is disabled";try{var r=el.getBoundingClientRect();if(!(r.width||r.height))return "the element has no size on screen";var top=document.elementFromPoint(r.left+r.width/2,r.top+r.height/2);if(top&&top!==el&&!el.contains(top)&&!top.contains(el)&&!(top.closest&&top.closest("[data-digitorn]"))){return "another element ("+top.tagName.toLowerCase()+(top.className&&typeof top.className==="string"?"."+top.className.split(" ")[0]:"")+") covers it"}}catch(e){}return ""}function human(el){reach(el);var r=el.getBoundingClientRect();var o={bubbles:true,cancelable:true,clientX:r.left+r.width/2,clientY:r.top+r.height/2,button:0};function fire(t,C){try{el.dispatchEvent(new (C||MouseEvent)(t,o))}catch(e){try{var ev=document.createEvent("Event");ev.initEvent(t,true,true);el.dispatchEvent(ev)}catch(_){}}}var P=(typeof PointerEvent!=="undefined")?PointerEvent:MouseEvent;fire("pointerover",P);fire("mouseover");fire("pointermove",P);fire("mousemove");fire("pointerdown",P);fire("mousedown");try{el.focus&&el.focus()}catch(e){}fire("pointerup",P);fire("mouseup");fire("click")}function typeHuman(el,txt){reach(el);try{el.focus&&el.focus()}catch(e){}var proto=el.tagName==="TEXTAREA"?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;var d=Object.getOwnPropertyDescriptor(proto,"value");var set=function(v){if(d&&d.set){d.set.call(el,v)}else{el.value=v}};set("");var s2=String(txt).slice(0,200);for(var i=0;i<s2.length;i++){var ch=s2.charAt(i);var ko={key:ch,bubbles:true,cancelable:true};try{el.dispatchEvent(new KeyboardEvent("keydown",ko))}catch(e){}try{el.dispatchEvent(new InputEvent("beforeinput",{data:ch,inputType:"insertText",bubbles:true,cancelable:true}))}catch(e){}set(el.value+ch);try{el.dispatchEvent(new InputEvent("input",{data:ch,inputType:"insertText",bubbles:true}))}catch(e){el.dispatchEvent(new Event("input",{bubbles:true}))}try{el.dispatchEvent(new KeyboardEvent("keyup",ko))}catch(e){}}el.dispatchEvent(new Event("change",{bubbles:true}))}function run(c){if(c.do==="observe")return 0;var el=(c.ref||c.text_match)?find(c):null;mark(el);if(c.do==="navigate"){var u=String(c.url||"");if(u.charAt(0)==="#"){location.hash=u}else if(u.charAt(0)==="/"){location.hash="#"+u}else{location.href=u}return 500}if(c.do==="wait"){return c.timeout>0?c.timeout:500}if(c.do==="scroll"){var to=String(c.text||"bottom");if(to==="top"){window.scrollTo({top:0,behavior:"smooth"})}else if(el){el.scrollIntoView({behavior:"smooth",block:"center"})}else{window.scrollTo({top:document.body.scrollHeight,behavior:"smooth"})}return 600}if(c.do==="detail"){if(!el)throw new Error("no element matching that ref or text");pendingDetail=c.ref;return 100}if(c.do==="viewport"){try{if(window.parent&&window.parent!==window)window.parent.postMessage({type:"digi:viewport",size:String(c.text||"mobile")},"*")}catch(e){}return 900}if(c.do==="wait_for"){var needle=String(c.text||""),limit=Date.now()+(c.timeout>0?c.timeout:8000);waitFor(needle,limit,c.id);return -1}if(!el)throw new Error("could not find "+(c.text_match?("\""+c.text_match+"\""):("ref "+c.ref))+" on the page — inspect again, it re-rendered");if(c.do==="click"){var why=reachable(el);if(why)throw new Error("a user could not click this: "+why);human(el);return 400}if(c.do==="type"){typeHuman(el,c.text||"");return 250}if(c.do==="press"){var k=String(c.key||"").toLowerCase();var K=k==="enter"?"Enter":k==="tab"?"Tab":k==="escape"?"Escape":c.key;var o={key:K,bubbles:true,cancelable:true};el.dispatchEvent(new KeyboardEvent("keydown",o));try{el.dispatchEvent(new KeyboardEvent("keypress",o))}catch(e){}el.dispatchEvent(new KeyboardEvent("keyup",o));if(K==="Enter"&&el.form&&el.form.requestSubmit){try{el.form.requestSubmit()}catch(e){}}return 500}if(c.do==="hover"){reach(el);var ev=["pointerover","mouseover","mouseenter","pointermove","mousemove"];for(var i=0;i<ev.length;i++){try{el.dispatchEvent(new MouseEvent(ev[i],{bubbles:true}))}catch(e){}}return 400}if(c.do==="check"){el.focus&&el.focus();if(el.checked===undefined){el.click();return 400}if(!el.checked){var d=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"checked");if(d&&d.set){d.set.call(el,true)}else{el.checked=true}el.dispatchEvent(new Event("click",{bubbles:true}));el.dispatchEvent(new Event("change",{bubbles:true}))}return 300}if(c.do==="select"){var want=String(c.text||"");var hit=-1;for(var j=0;j<el.options.length;j++){var o=el.options[j];if(o.text===want||o.value===want){hit=j;break}}if(hit<0)throw new Error("no option "+want);el.selectedIndex=hit;el.dispatchEvent(new Event("change",{bubbles:true}));return 300}throw new Error("unknown action "+c.do)}function exec(cmds){ui();var i=0;(function next(){if(i>=cmds.length){done();return}var c=cmds[i++],d=300;try{d=run(c)}catch(e){push("error",String(e&&e.message||e),e&&e.stack,"",0,0)}if(d<0){next();return}setTimeout(function(){post(c.id,snap()).then(function(r){if(r&&r.commands&&r.commands.length){cmds=cmds.concat(r.commands)}next()},next)},d)})()}var stop=false;function tick(){if(busy||stop)return;busy=true;post("",snap()).then(function(r){busy=false;fresh=false;if(r&&r.commands&&r.commands.length){exec(r.commands)}setTimeout(tick,50)},function(){busy=false;setTimeout(tick,3000)})}var pend=0;function soon(){if(busy)return;clearTimeout(pend);pend=setTimeout(tick,120)}addEventListener("pagehide",function(){stop=true});document.addEventListener("visibilitychange",function(){if(!document.hidden){stop=false;soon()}});if(document.readyState==="loading"){addEventListener("DOMContentLoaded",function(){setTimeout(tick,300)})}else{setTimeout(tick,300)}}catch(e){}})();</script>`

// injectThemeShim inserts the theme + error + nav shims into an HTML document —
// right before </head> when present, else after the opening <head>, else prepended.
func injectThemeShim(html []byte) []byte {
	shim := []byte(previewThemeShim + previewErrorShim + previewNavShim + previewAgentShim)
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
