package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// livenessHandler answers /health and /ready with a 200. The daemon is
// "live" the instant the router serves traffic — Build() + Start() ran
// to completion (DB migrated, session store up, workers spawned, HTTP
// listener bound) before this handler can fire. Returning 200 is a
// statement of fact about the daemon process, not a deep healthcheck
// (per-subsystem probes live under /api/daemon/stats).
func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// MountAPI registers every public REST route on the daemon's router.
// Routes preserve the legacy Python daemon contract (/api/apps/...).
// Routes that require the runtime (preview, fork, undo, …) are registered
// as stubs returning 501 so clients see a consistent API surface.
func (d *Daemon) MountAPI() {
	r := d.httpSrv.Router()

	// ----- Liveness / readiness (unauthenticated) -----
	// Both endpoints return 200 once the HTTP server is serving — the
	// daemon is by definition past Build() + Start() initialization
	// because the router is already listening when this handler fires.
	// Used by orchestrators (PowerShell live test, k8s probes, smoke
	// tests) to know when the daemon is reachable.
	r.Get("/health", livenessHandler)
	r.Get("/ready", livenessHandler)

	// ----- Preview static serve (unauthenticated, token-gated) -----
	// The Preview iframe loads this URL by direct browser navigation, so it can
	// NOT carry the JWT. Authorization is the per-session `?t=` token minted by
	// the authenticated /web-preview resolver; the handler confines every path to
	// the session workdir (PathPolicy) and refuses the shadow repo. It serves the
	// agent's BUILT output — the daemon never runs a build or a dev server.
	r.With(d.panicRecoverer).Get(
		"/api/apps/{app_id}/sessions/{session_id}/preview/serve/*",
		d.servePreviewFile,
	)
	// 404 fallback: serve the active preview's ROOT-absolute assets (/assets/* …)
	// so a default Vite/CRA build renders in the iframe instead of going blank.
	// Falls through to a normal 404 when no preview is active.
	r.NotFound(d.previewRootFallback)

	// ----- MCP OAuth callback (unauthenticated, state-gated) -----
	// Hit by the provider's browser redirect, which cannot carry the JWT.
	// Authorization is the single-use state→user binding persisted at authorize
	// time; the handler exchanges the code and stores the token server-side.
	r.With(d.panicRecoverer).Get("/api/apps/{app_id}/oauth/callback", d.mcpOAuthCallback)

	r.Group(func(r chi.Router) {
		r.Use(d.panicRecoverer) // first: no handler panic ever escapes (jamais crash)
		if d.jwtVerifier != nil {
			r.Use(jwtAuthMiddleware(d.jwtVerifier, d.cfg.Auth.DevMode))
		}
		r.Use(authMiddleware)
		r.Use(d.actAsMiddleware) // trusted on-behalf-of (X-Act-As-User) → effective user

		// ----- Sessions -----
		r.Route("/api/apps/{app_id}/sessions", func(r chi.Router) {
			r.Get("/", d.listSessions)
			r.Post("/", d.createSession)
			r.Get("/search", d.searchSessions)

			r.Route("/{session_id}", func(r chi.Router) {
				r.Get("/", d.getSession)
				r.Patch("/", d.renameSession)
				r.Delete("/", d.deleteSession)
				r.Get("/model", d.getSessionModel)
				r.Put("/model", d.putSessionModel)
				r.Get("/history", d.getHistory)
				r.Get("/events", d.getEvents)
				r.Get("/state", d.getState)
				r.Get("/memory", d.getMemory)
				r.Get("/agents", d.getAgents)
				r.Get("/queue", d.getQueue)

				r.Post("/messages", d.postMessage)
				r.Post("/abort", d.abortTurn)
				r.Post("/compact", d.compactSession)

				// background_run tasks for this session : list + cancel.
				// Real-time task lifecycle is pushed via the background_task
				// events on the Socket.IO bridge ; these give the client a
				// snapshot + an explicit cancel control.
				r.Get("/tasks", d.listBackgroundTasks)
				r.Post("/tasks/{task_id}/cancel", d.cancelBackgroundTask)
				r.Post("/resume", func(w http.ResponseWriter, r *http.Request) { notImplemented(w, "session.resume") })
				r.Post("/undo", func(w http.ResponseWriter, r *http.Request) { notImplemented(w, "session.undo") })
				r.Post("/fork", d.forkSession)
				r.Get("/export", d.exportSession)
				r.Get("/preview", func(w http.ResponseWriter, r *http.Request) { notImplemented(w, "session.preview") })
				r.Get("/images/{image_id}", func(w http.ResponseWriter, r *http.Request) { notImplemented(w, "session.images") })
			})
		})

		// ----- Approvals -----
		r.Get("/api/apps/{app_id}/approvals", d.listApprovals)
		r.Post("/api/apps/{app_id}/approve", d.resolveApproval)

		// ----- Secrets (in-memory V1, file-backed V2) -----
		r.Get("/api/apps/{app_id}/required-secrets", d.requiredSecrets)
		r.Get("/api/apps/{app_id}/secrets", d.listSecrets)
		r.Get("/api/apps/{app_id}/secrets/{key}", d.getSecret)
		r.Put("/api/apps/{app_id}/secrets", d.setSecrets)
		r.Put("/api/apps/{app_id}/secrets/{key}", d.setSecret)
		r.Delete("/api/apps/{app_id}/secrets/{key}", d.deleteSecret)

		// ----- Diagnostics / Status -----
		r.Get("/api/apps/{app_id}/diagnostics", d.diagnostics)
		r.Get("/api/apps/{app_id}/status", d.appStatus)
		r.Get("/api/apps/{app_id}/errors", d.appErrors)
		r.Get("/api/apps/{app_id}/ui-config", d.uiConfig)
		r.Get("/api/apps/{app_id}/deploy-status", d.deployStatus)

		// ----- Daemon stats / observability -----
		r.Get("/api/daemon/stats", d.daemonStats)

		// ----- App manager (install, list, get, lifecycle, serving) -----
		d.mountAppRoutes(r)

		// ----- Dev-only debug routes (gated by cfg.Auth.DevMode) -----
		if d.cfg.Auth.DevMode {
			r.Post("/api/_dev/invoke", d.devInvoke)
		}

		// ----- Stubs for routes requiring runtime / deployment subsystems -----
		d.mountStubs(r)
	})
}

// mountAppRoutes wires every /api/apps/* route handled by the App
// Manager. Routes preserve the Python daemon names so existing clients
// keep working ; routes we intentionally dropped are documented in
// mountStubs() and return 501.
func (d *Daemon) mountAppRoutes(r chi.Router) {
	// Lifecycle.
	r.Post("/api/apps/install", d.installApp)
	r.Post("/api/apps/{app_id}/upgrade", d.upgradeApp)
	r.Post("/api/apps/{app_id}/uninstall", d.uninstallApp)
	r.Delete("/api/apps/{app_id}", d.uninstallApp) // alias for client compat
	r.Post("/api/apps/{app_id}/enable", d.enableApp)
	r.Post("/api/apps/{app_id}/disable", d.disableApp)
	r.Put("/api/apps/{app_id}/byok", d.setAppBYOK)
	r.Post("/api/apps/{app_id}/reload", d.reloadApp)
	r.Get("/api/apps/{app_id}/check-update", d.checkUpdate)

	// Read.
	r.Get("/api/apps", d.listApps)
	r.Get("/api/apps/disabled", d.listDisabledApps)
	r.Get("/api/apps/{app_id}", d.getApp)
	r.Get("/api/apps/{app_id}/manifest", d.getManifest)

	// Serving.
	r.Get("/api/apps/{app_id}/icon", d.serveIcon)
	r.Get("/api/apps/{app_id}/files", d.listFiles)
	r.Get("/api/apps/{app_id}/assets/*", d.serveAsset)
	r.Get("/api/apps/{app_id}/index", d.getIndex)
}

// mountStubs registers every legacy-daemon route that we cannot yet
// implement, returning 501 with a structured body so the client surface
// is identical to the Python daemon's.
func (d *Daemon) mountStubs(r chi.Router) {
	stub := func(feature string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { notImplemented(w, feature) }
	}

	// Voice — the daemon-side audio endpoint (WebSocket). The digitorn-voice adapter
	// streams the call's PCM here; STT, the agent turn (gateway LLM + tools + gates),
	// and TTS all run in the daemon (api_voice.go). The daemon IS the brain.
	r.Get("/api/apps/{app_id}/sessions/{session_id}/voice/audio", d.voiceAudioWS)

	// Workspace — git-backed change tracking over the session workdir (real,
	// see api_workspace.go). The rest stay stubs until later bricks.
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace", stub("workspace.list"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/changes", d.getWorkspaceChanges)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/diff", d.getWorkspaceDiff)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/tree", d.getWorkspaceTree)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/search", d.getWorkspaceSearch)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/code-snapshot", stub("workspace.code_snapshot"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/preview-snapshot", stub("workspace.preview_snapshot"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/export", stub("workspace.export"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/history", d.getWorkspaceHistory)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/revert", d.postWorkspaceRevert)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/files/{filepath}/history", d.getWorkspaceFileHistory)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/{filepath}/revert", d.postWorkspaceFileRevert)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/files/*", d.getWorkspaceFile)
	r.Put("/api/apps/{app_id}/sessions/{session_id}/workspace/files/*", d.putWorkspaceFile)
	r.Delete("/api/apps/{app_id}/sessions/{session_id}/workspace/files/*", stub("workspace.files_delete"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/approve", d.postWorkspaceApprove)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/approve-all", d.postWorkspaceApproveAll)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/approve-hunks", d.postWorkspaceApproveHunks)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/reject", d.postWorkspaceReject)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/reject-hunks", d.postWorkspaceRejectHunks)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/commit", d.postWorkspaceCommit)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/fork", stub("workspace.fork"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/import", stub("workspace.import"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/upload/*", stub("workspace.upload"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/git-status", stub("workspace.git_status"))

	// Tools — needs runtime / module registry plug.
	r.Get("/api/apps/{app_id}/tools/search", stub("tools.search"))
	r.Get("/api/apps/{app_id}/tools/categories", stub("tools.categories"))
	r.Get("/api/apps/{app_id}/tools/categories/{category}", stub("tools.category"))
	r.Get("/api/apps/{app_id}/tools/{tool_name}", stub("tools.get"))
	r.Post("/api/apps/{app_id}/tools/{tool_name}/execute", stub("tools.execute"))

	// Widgets.
	r.Get("/api/apps/{app_id}/widgets", stub("widgets.tree"))
	r.Get("/api/apps/{app_id}/widgets/data/{binding}", stub("widgets.data"))
	r.Get("/api/apps/{app_id}/widgets/data/{binding}/stream", stub("widgets.data_stream"))
	r.Post("/api/apps/{app_id}/widgets/action", stub("widgets.action"))
	r.Post("/api/apps/{app_id}/widgets/upload", stub("widgets.upload"))
	r.Get("/api/apps/{app_id}/widgets/upload/{user_id}/{sid}/{file_id}/{filename}", stub("widgets.download"))
	r.Get("/api/apps/{app_id}/widgets/validate", stub("widgets.validate"))
	r.Post("/api/apps/{app_id}/interact", stub("interact.generic"))

	// Background tasks / sessions / activations.
	r.Get("/api/apps/{app_id}/background-tasks", stub("background.list"))
	r.Get("/api/apps/{app_id}/background-tasks/{task_id}", stub("background.get"))
	r.Post("/api/apps/{app_id}/background-tasks", stub("background.create"))
	r.Post("/api/apps/{app_id}/background-tasks/{task_id}/wait", stub("background.wait"))
	r.Delete("/api/apps/{app_id}/background-tasks/{task_id}", stub("background.delete"))
	// /sessions/{session_id}/tasks (list) and .../cancel are wired to the
	// real background.Manager in MountAPI's session route group.
	r.Get("/api/apps/{app_id}/background-sessions", stub("bgsessions.list"))
	r.Post("/api/apps/{app_id}/background-sessions", stub("bgsessions.create"))
	r.Get("/api/apps/{app_id}/background-sessions/{bg_session_id}", stub("bgsessions.get"))
	r.Get("/api/apps/{app_id}/background-sessions/{bg_session_id}/payload", stub("bgsessions.payload"))
	r.Put("/api/apps/{app_id}/background-sessions/{bg_session_id}/payload", stub("bgsessions.set_payload"))
	r.Delete("/api/apps/{app_id}/background-sessions/{bg_session_id}/payload", stub("bgsessions.clear_payload"))
	r.Post("/api/apps/{app_id}/background-sessions/{bg_session_id}/pause", stub("bgsessions.pause"))
	r.Post("/api/apps/{app_id}/background-sessions/{bg_session_id}/resume", stub("bgsessions.resume"))
	r.Delete("/api/apps/{app_id}/background-sessions/{bg_session_id}", stub("bgsessions.delete"))
	r.Get("/api/apps/{app_id}/activations", stub("activations.list"))
	r.Get("/api/apps/{app_id}/activations/stats", stub("activations.stats"))
	r.Get("/api/apps/{app_id}/activations/{activation_id}", stub("activations.get"))
	r.Get("/api/apps/{app_id}/artifacts/{event_id}/download", stub("artifacts.download"))
	r.Head("/api/apps/{app_id}/artifacts/{event_id}/download", stub("artifacts.head"))
	r.Get("/api/apps/{app_id}/notifications/active", stub("notifications.active"))
	r.Post("/api/apps/{app_id}/notifications", stub("notifications.send"))

	// Skills / Snippets / Templates / Triggers / Watchers / Preview / LSP / OAuth-MCP.
	r.Get("/api/apps/{app_id}/templates", stub("templates.list"))
	r.Get("/api/apps/{app_id}/skills", d.listSkills)
	r.Post("/api/apps/{app_id}/skills", d.createSkill)
	r.Patch("/api/apps/{app_id}/skills/{skill_id}", d.updateSkill)
	r.Delete("/api/apps/{app_id}/skills/{skill_id}", d.deleteSkill)
	r.Get("/api/apps/{app_id}/snippets", d.listSnippets)
	r.Post("/api/apps/{app_id}/snippets", d.createSnippet)
	r.Patch("/api/apps/{app_id}/snippets/{snippet_id}", d.updateSnippet)
	r.Delete("/api/apps/{app_id}/snippets/{snippet_id}", d.deleteSnippet)
	r.Get("/api/apps/{app_id}/triggers", stub("triggers.list"))
	r.Post("/api/apps/{app_id}/triggers/{trigger_id}/fire", stub("triggers.fire"))
	r.Post("/api/apps/{app_id}/triggers/{trigger_id}/test", stub("triggers.test"))
	r.Get("/api/apps/{app_id}/watchers", stub("watchers.list"))
	r.Get("/api/apps/{app_id}/watchers/{watcher_id}", stub("watchers.get"))
	r.Post("/api/apps/{app_id}/watchers", stub("watchers.create"))
	r.Post("/api/apps/{app_id}/watchers/{watcher_id}/pause", stub("watchers.pause"))
	r.Post("/api/apps/{app_id}/watchers/{watcher_id}/resume", stub("watchers.resume"))
	r.Delete("/api/apps/{app_id}/watchers/{watcher_id}", stub("watchers.delete"))
	r.Get("/api/apps/{app_id}/preview-bootstrap", stub("preview.bootstrap"))
	r.Get("/api/apps/{app_id}/web-preview", d.getWebPreview)
	r.Post("/api/apps/{app_id}/sessions/{session_id}/lsp", stub("lsp.rpc"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/lsp-cancel", stub("lsp.cancel"))
	// MCP OAuth (real handlers; callback is registered unauthenticated in MountAPI).
	r.Get("/api/apps/{app_id}/oauth/authorize", d.mcpOAuthAuthorize)
	r.Post("/api/apps/{app_id}/mcp/{server_id}/oauth-token", d.mcpOAuthTokenSet)
	r.Delete("/api/apps/{app_id}/mcp/{server_id}/oauth-token", d.mcpOAuthTokenRevoke)
	r.Get("/api/apps/{app_id}/mcp/pending-oauth", d.mcpPendingOAuth)
	r.Get("/api/apps/{app_id}/payload-schema", stub("apps.payload_schema"))
	r.Get("/api/apps/{app_id}/hooks", stub("apps.hooks"))
	r.Get("/api/apps/{app_id}/channels/health", stub("apps.channels_health"))
}
