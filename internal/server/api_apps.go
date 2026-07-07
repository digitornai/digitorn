package server

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// osReadDir aliases os.ReadDir so we keep the os dependency local to
// this file without sprinkling os.* across handlers.
func osReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }

// ---------- Lifecycle ----------

type installRequest struct {
	Source string `json:"source"`
}

type installResponse struct {
	AppID      string `json:"app_id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Source     string `json:"source"`
	InstallDir string `json:"install_dir"`
	Enabled    bool   `json:"enabled"`
	BYOK       bool   `json:"byok"`
}

// installApp handles POST /api/apps/install. Body : {"source": "..."}.
// Source can be a local filesystem path, hub://publisher/pkg@version
// or builtin://name. The user's JWT (if present) is forwarded to the
// hub for authenticated downloads.
func (d *Daemon) installApp(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "source required")
		return
	}
	// Rebuild the module catalog from the live registry : worker-hosted module
	// manifests arrive asynchronously after boot, so a catalog cached during
	// startup can miss them and wrongly reject an app as "unknown module".
	if d.appCompiler != nil {
		d.appCompiler.InvalidateCatalog()
	}
	app, err := d.appMgr.Install(r.Context(), req.Source, bearerToken(r))
	if err != nil {
		writeError(w, appMgrErrStatus(err), "install_failed", err.Error())
		return
	}
	// Drop the cached per-agent tool index for this app. It is keyed on the app's
	// YAML `version`, so re-installing at the same version (the common dev loop)
	// would otherwise keep serving the STALE toolset built from the previous
	// bundle — a new session would see the old tools (or none) until a daemon
	// restart. Same call `setAppPieces` already makes; every mutation of an app's
	// definition must invalidate this cache.
	if d.promptBuilder != nil {
		d.promptBuilder.Invalidate(app.AppID, "", "")
	}
	go func(app *appmgr.App) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		d.pushTriggersToBackground(ctx, app)
	}(app)
	writeJSON(w, http.StatusOK, installResponse{
		AppID: app.AppID, Name: app.Name, Version: app.Version,
		Source: req.Source, InstallDir: filepath.Join(d.cfg.Apps.Root, app.AppID),
		Enabled: app.Enabled, BYOK: app.BYOK,
	})
}

// upgradeApp handles POST /api/apps/{app_id}/upgrade.
func (d *Daemon) upgradeApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	var req installRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	app, err := d.appMgr.Upgrade(r.Context(), appID, req.Source, bearerToken(r))
	if err != nil {
		writeError(w, appMgrErrStatus(err), "upgrade_failed", err.Error())
		return
	}
	if d.promptBuilder != nil {
		d.promptBuilder.Invalidate(appID, "", "")
	}
	writeJSON(w, http.StatusOK, installResponse{
		AppID: app.AppID, Name: app.Name, Version: app.Version,
		Source: req.Source, InstallDir: filepath.Join(d.cfg.Apps.Root, app.AppID),
		Enabled: app.Enabled, BYOK: app.BYOK,
	})
}

// uninstallApp handles POST /api/apps/{app_id}/uninstall and the
// legacy DELETE /api/apps/{app_id} alias. Query param ?purge=true|false
// signals whether the caller wants associated sessions wiped too — for
// V1 we just record the intent ; sessionstore purge ties into A3 later.
func (d *Daemon) uninstallApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	purge := r.URL.Query().Get("purge") == "true"
	if err := d.appMgr.Uninstall(r.Context(), appID, purge); err != nil {
		writeError(w, appMgrErrStatus(err), "uninstall_failed", err.Error())
		return
	}
	if d.promptBuilder != nil {
		d.promptBuilder.Invalidate(appID, "", "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id":      appID,
		"uninstalled": true,
		"purge":       purge,
	})
}

// enableApp handles POST /api/apps/{app_id}/enable.
func (d *Daemon) enableApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if err := d.appMgr.Enable(r.Context(), appID); err != nil {
		writeError(w, appMgrErrStatus(err), "enable_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app_id": appID, "enabled": true})
}

// disableApp handles POST /api/apps/{app_id}/disable.
func (d *Daemon) disableApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if err := d.appMgr.Disable(r.Context(), appID); err != nil {
		writeError(w, appMgrErrStatus(err), "disable_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app_id": appID, "enabled": false})
}

// setAppBYOK handles PUT /api/apps/{app_id}/byok with body
// {"enabled": bool}. Toggles whether this app's LLM traffic dials the
// provider directly using the brain-declared credential (true) or
// routes through the digitorn LLM gateway with UserJWT (false).
// Persisted across daemon restarts ; survives bundle re-installs.
func (d *Daemon) setAppBYOK(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := d.appMgr.SetBYOK(r.Context(), appID, body.Enabled); err != nil {
		writeError(w, appMgrErrStatus(err), "set_byok_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id": appID,
		"byok":   body.Enabled,
	})
}

func (d *Daemon) setAppPieces(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	var body struct {
		Pieces []string `json:"pieces"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if pm := d.piecesModule(); pm != nil && pm.PiecesStore() != nil {
		userID := userIDOf(r.Context())
		store := pm.PiecesStore()
		for _, p := range body.Pieces {
			view, ok, err := store.Get(r.Context(), userID, p)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "check_failed", err.Error())
				return
			}
			if !ok || len(view.SecretKeys) == 0 {
				writeError(w, http.StatusBadRequest, "not_configured",
					"connector \""+p+"\" must be configured before it can be associated with an app")
				return
			}
		}
	}
	if err := d.appMgr.SetAppPieces(r.Context(), appID, body.Pieces); err != nil {
		writeError(w, appMgrErrStatus(err), "set_pieces_failed", err.Error())
		return
	}
	if d.piecesCatalog != nil {
		d.piecesCatalog.invalidate(appID)
	}
	if d.promptBuilder != nil {
		d.promptBuilder.Invalidate(appID, "", "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id": appID,
		"pieces": body.Pieces,
	})
}

func (d *Daemon) getAppPieces(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "get_pieces_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id": appID,
		"pieces": manifestAllowedPieces(def),
	})
}

func manifestAllowedPieces(def *schema.AppDefinition) []string {
	out := []string{}
	if def == nil || def.Tools == nil {
		return out
	}
	mb, ok := def.Tools.Modules["pieces"]
	if !ok || mb.Constraints == nil {
		return out
	}
	raw, ok := mb.Constraints["allowed_pieces"]
	if !ok {
		return out
	}
	switch v := raw.(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// setAppDisplayName overrides the displayed label for an app. An empty/blank
// name clears the override (falls back to the bundle's short name). Returns the
// new effective short name.
func (d *Daemon) setAppDisplayName(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := d.appMgr.SetDisplayName(r.Context(), appID, body.Name); err != nil {
		writeError(w, appMgrErrStatus(err), "set_display_name_failed", err.Error())
		return
	}
	short := strings.TrimSpace(body.Name)
	if ra, err := d.appMgr.Get(r.Context(), appID); err == nil && ra != nil && ra.Meta != nil {
		short = ra.Meta.ShortName
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id":     appID,
		"short_name": short,
	})
}

// reloadApp handles POST /api/apps/{app_id}/reload : recompile from
// the on-disk source. Used after the operator hand-edits app.yaml.
func (d *Daemon) reloadApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if err := d.appMgr.Reload(r.Context(), appID); err != nil {
		writeError(w, appMgrErrStatus(err), "reload_failed", err.Error())
		return
	}
	// Reload recompiled the definition from disk; drop the stale tool-index cache
	// so the next session rebuilds from it (the cache key's app version doesn't
	// change on a same-version hand-edit + reload).
	if d.promptBuilder != nil {
		d.promptBuilder.Invalidate(appID, "", "")
	}
	app, err := d.appMgr.GetApp(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "reload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// checkUpdate handles GET /api/apps/{app_id}/check-update : asks the
// hub for the latest version of an app installed from hub://.
func (d *Daemon) checkUpdate(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	info, err := d.appMgr.CheckUpdate(r.Context(), appID, bearerToken(r))
	if err != nil {
		writeError(w, appMgrErrStatus(err), "check_update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// ---------- Read ----------

// listApps handles GET /api/apps. Query ?include_disabled=true
// returns disabled apps too. Default : enabled only.
func (d *Daemon) listApps(w http.ResponseWriter, r *http.Request) {
	includeDisabled := r.URL.Query().Get("include_disabled") == "true"
	apps, err := d.appMgr.List(r.Context(), includeDisabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	// `success`+`data` is the envelope the web client expects; `apps`+`count`
	// are kept for the CLI and existing API consumers.
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": apps, "apps": apps, "count": len(apps)})
}

// listDisabledApps handles GET /api/apps/disabled.
func (d *Daemon) listDisabledApps(w http.ResponseWriter, r *http.Request) {
	apps, err := d.appMgr.ListDisabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": apps, "apps": apps, "count": len(apps)})
}

// getApp handles GET /api/apps/{app_id}.
func (d *Daemon) getApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	app, err := d.appMgr.GetApp(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// getManifest handles GET /api/apps/{app_id}/manifest. Returns the
// compiled AppDefinition for marketplace UIs that want the full tool
// catalogue and agent list.
func (d *Daemon) getManifest(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "manifest_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, def)
}

// ---------- Serving ----------

// serveIcon handles GET /api/apps/{app_id}/icon. Three resolution
// modes, in order :
//  1. App.Icon ends with .png/.svg/.jpg/.jpeg/.gif/.webp/.ico → serve
//     {bundle}/assets/{Icon} as a file
//  2. App.Icon empty → serve {bundle}/assets/icon.* if present
//  3. App.Icon is text/emoji → render an inline SVG with the text
//     centered on a rounded square coloured by App.Color
func (d *Daemon) serveIcon(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	app, err := d.appMgr.GetApp(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "icon_failed", err.Error())
		return
	}
	bundle := filepath.Join(d.cfg.Apps.Root, appID)

	// Mode 1 : file reference with image extension.
	if app.Icon != "" && isImageRef(app.Icon) {
		serveBundleFile(w, r, bundle, filepath.Join(bundle, "assets", app.Icon))
		return
	}
	// Mode 3 : text / emoji → render SVG.
	if app.Icon != "" {
		svg := renderTextIconSVG(app.Icon, app.Color)
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(svg))
		return
	}
	// Mode 2 : empty Icon → fallback to assets/icon.*.
	matches, _ := filepath.Glob(filepath.Join(bundle, "assets", "icon.*"))
	if len(matches) > 0 {
		serveBundleFile(w, r, bundle, matches[0])
		return
	}
	// No image file shipped with the app. This 404 is the contract, not a
	// failure : the client (which already holds the app's icon/colour metadata)
	// falls back to the declared emoji or the name's initial. The daemon does
	// not synthesise an image — presentation is the client's call.
	writeError(w, http.StatusNotFound, "icon_not_found", "no icon file")
}

// isImageRef returns true if s has a file extension we recognize as
// an image, indicating App.Icon points to a file rather than being
// text/emoji content.
func isImageRef(s string) bool {
	ext := strings.ToLower(filepath.Ext(s))
	switch ext {
	case ".png", ".svg", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".avif":
		return true
	}
	return false
}

// renderTextIconSVG produces a 64×64 SVG with `text` centered on a
// rounded square coloured by `hexColor` (defaults to indigo when
// empty). Used for emoji or short-text app icons. The SVG is built
// by hand because the input length is tiny and we want to avoid
// pulling in a templating engine for one string.
func renderTextIconSVG(text, hexColor string) string {
	fg := strings.TrimSpace(hexColor)
	if fg == "" {
		fg = "#6366F1"
	}
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">` +
		`<text x="32" y="32" text-anchor="middle" dominant-baseline="central" ` +
		`font-size="40" font-family="system-ui, -apple-system, &quot;Segoe UI&quot;, sans-serif" ` +
		`fill="` + fg + `">` + escapeXMLContent(text) + `</text></svg>`
}

// escapeXMLContent escapes the 5 characters that need escaping in XML
// text content. We don't use html.EscapeString because that targets
// HTML attributes too aggressively.
func escapeXMLContent(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// listFiles handles GET /api/apps/{app_id}/files?subdir=... It lists
// the bundle dir contents (or a subdir) for debugging / file-tree UIs.
func (d *Daemon) listFiles(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if _, err := d.appMgr.GetApp(r.Context(), appID); err != nil {
		writeError(w, appMgrErrStatus(err), "files_failed", err.Error())
		return
	}
	bundle := filepath.Join(d.cfg.Apps.Root, appID)
	subdir := r.URL.Query().Get("subdir")
	target := bundle
	if subdir != "" {
		target = filepath.Join(bundle, filepath.Clean(subdir))
		if !strings.HasPrefix(target, bundle) {
			writeError(w, http.StatusForbidden, "forbidden", "subdir escapes bundle")
			return
		}
	}
	entries, err := listDir(target)
	if err != nil {
		writeError(w, http.StatusNotFound, "files_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id":  appID,
		"subdir":  subdir,
		"entries": entries,
	})
}

// serveAsset handles GET /api/apps/{app_id}/assets/* — serves any
// file under {bundle}/assets/.
func (d *Daemon) serveAsset(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	rest := chi.URLParam(r, "*")
	if rest == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "asset path required")
		return
	}
	if _, err := d.appMgr.GetApp(r.Context(), appID); err != nil {
		writeError(w, appMgrErrStatus(err), "asset_failed", err.Error())
		return
	}
	bundle := filepath.Join(d.cfg.Apps.Root, appID)
	target := filepath.Join(bundle, "assets", filepath.Clean(rest))
	if !strings.HasPrefix(target, filepath.Join(bundle, "assets")) {
		writeError(w, http.StatusForbidden, "forbidden", "asset path escapes assets dir")
		return
	}
	serveBundleFile(w, r, bundle, target)
}

// serveAppWeb serves an app's EMBEDDED preview UI — the built static bundle the
// app ships in its install dir at {app}/web/dist/. It is served once and SHARED
// by every session of the app (one build, never copied per-workdir), which is
// the whole point: 10k sessions do NOT mean 10k preview copies. Distinct from
// the agent-built workdir preview (preview/serve). The bundle is static,
// non-secret app UI code; the in-page SDK carries the session context and
// handles per-session authorization. A missing or directory path serves
// index.html so client-side routing works (SPA).
func (d *Daemon) serveAppWeb(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if _, err := d.appMgr.GetApp(r.Context(), appID); err != nil {
		writeError(w, appMgrErrStatus(err), "web_failed", err.Error())
		return
	}
	bundle := filepath.Join(d.cfg.Apps.Root, appID)
	root := filepath.Join(bundle, "web", "dist")
	rest := strings.TrimPrefix(filepath.Clean("/"+chi.URLParam(r, "*")), "/")
	if rest == "" || rest == "." {
		rest = "index.html"
	}
	target := filepath.Join(root, rest)
	if !strings.HasPrefix(target, root) {
		writeError(w, http.StatusForbidden, "forbidden", "path escapes web dir")
		return
	}
	// SPA fallback: unknown non-asset path → index.html.
	if fi, err := os.Stat(target); err != nil || fi.IsDir() {
		target = filepath.Join(root, "index.html")
	}
	serveBundleFile(w, r, bundle, target)
}

// getIndex handles GET /api/apps/{app_id}/index : returns the tool
// catalogue (modules + tools) from the compiled manifest.
func (d *Daemon) getIndex(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "index_failed", err.Error())
		return
	}
	out := map[string]any{
		"app_id":  appID,
		"agents":  def.Agents,
		"tools":   def.Tools,
		"runtime": def.Runtime,
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- helpers ----------

// appMgrErrStatus maps appmgr typed errors to HTTP statuses.
func appMgrErrStatus(err error) int {
	switch {
	case errors.Is(err, appmgr.ErrAppNotFound):
		return http.StatusNotFound
	case errors.Is(err, appmgr.ErrAppDisabled):
		return http.StatusForbidden
	case errors.Is(err, appmgr.ErrSourceMissingYAML),
		errors.Is(err, appmgr.ErrAppIDMismatch),
		errors.Is(err, appmgr.ErrBadSource),
		errors.Is(err, appmgr.ErrCompileFailed):
		return http.StatusBadRequest
	case errors.Is(err, appmgr.ErrArchiveTooBig),
		errors.Is(err, appmgr.ErrArchiveTraversal):
		return http.StatusUnprocessableEntity
	case errors.Is(err, appmgr.ErrHubFetch):
		return http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// bearerToken extracts the JWT from the Authorization header (sans
// "Bearer " prefix). Returns "" if missing.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// serveBundleFile sends a file from inside the bundle dir with a
// safe content-type. The bundle param is the absolute install dir,
// used only to verify the file actually lives under it.
func serveBundleFile(w http.ResponseWriter, r *http.Request, bundle, path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "abs_failed", err.Error())
		return
	}
	bundleAbs, err := filepath.Abs(bundle)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "abs_failed", err.Error())
		return
	}
	if !strings.HasPrefix(abs, bundleAbs) {
		writeError(w, http.StatusForbidden, "forbidden", "path escapes bundle")
		return
	}
	// HTML entries get the platform theme shim injected so the app adopts the
	// host theme even if it doesn't use @digitorn/sdk.
	if isHTMLFile(abs) {
		serveHTMLWithShim(w, abs)
		return
	}
	// Set Content-Type from extension so /assets/foo.png serves PNG.
	if ct := mime.TypeByExtension(filepath.Ext(abs)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, abs)
}

// listDir produces a flat slice of {name, is_dir, size} for the
// immediate children of dir. Sorted lexicographically.
func listDir(dir string) ([]map[string]any, error) {
	entries, err := osReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{
			"name":   e.Name(),
			"is_dir": e.IsDir(),
		}
		if info, err := e.Info(); err == nil {
			row["size"] = strconv.FormatInt(info.Size(), 10)
		}
		out = append(out, row)
	}
	return out, nil
}
