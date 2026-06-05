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

	r.Group(func(r chi.Router) {
		if d.jwtVerifier != nil {
			r.Use(jwtAuthMiddleware(d.jwtVerifier, d.cfg.Auth.DevMode))
		}
		r.Use(authMiddleware)

		// ----- Sessions -----
		r.Route("/api/apps/{app_id}/sessions", func(r chi.Router) {
			r.Get("/", d.listSessions)
			r.Post("/", d.createSession)
			r.Get("/search", d.searchSessions)

			r.Route("/{session_id}", func(r chi.Router) {
				r.Get("/", d.getSession)
				r.Delete("/", d.deleteSession)
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

	// Workspace — git-backed change tracking over the session workdir (real,
	// see api_workspace.go). The rest stay stubs until later bricks.
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace", stub("workspace.list"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/changes", d.getWorkspaceChanges)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/diff", d.getWorkspaceDiff)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/tree", d.getWorkspaceTree)
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/code-snapshot", stub("workspace.code_snapshot"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/preview-snapshot", stub("workspace.preview_snapshot"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/export", stub("workspace.export"))
	r.Get("/api/apps/{app_id}/sessions/{session_id}/workspace/files/*", d.getWorkspaceFile)
	r.Put("/api/apps/{app_id}/sessions/{session_id}/workspace/files/*", stub("workspace.files_write"))
	r.Delete("/api/apps/{app_id}/sessions/{session_id}/workspace/files/*", stub("workspace.files_delete"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/approve", stub("workspace.approve"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/approve-hunks", stub("workspace.approve_hunks"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/reject", stub("workspace.reject"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/files/reject-hunks", stub("workspace.reject_hunks"))
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
	r.Get("/api/apps/{app_id}/skills", stub("skills.list"))
	r.Post("/api/apps/{app_id}/skills", stub("skills.create"))
	r.Patch("/api/apps/{app_id}/skills/{skill_id}", stub("skills.update"))
	r.Delete("/api/apps/{app_id}/skills/{skill_id}", stub("skills.delete"))
	r.Get("/api/apps/{app_id}/snippets", stub("snippets.list"))
	r.Post("/api/apps/{app_id}/snippets", stub("snippets.create"))
	r.Patch("/api/apps/{app_id}/snippets/{snippet_id}", stub("snippets.update"))
	r.Delete("/api/apps/{app_id}/snippets/{snippet_id}", stub("snippets.delete"))
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
	r.Get("/api/apps/{app_id}/web-preview", stub("preview.web"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/lsp", stub("lsp.rpc"))
	r.Post("/api/apps/{app_id}/sessions/{session_id}/lsp-cancel", stub("lsp.cancel"))
	r.Get("/api/apps/{app_id}/oauth/authorize", stub("oauth.authorize"))
	r.Get("/api/apps/{app_id}/oauth/callback", stub("oauth.callback"))
	r.Post("/api/apps/{app_id}/mcp/{server_id}/oauth-token", stub("mcp.oauth_token_set"))
	r.Delete("/api/apps/{app_id}/mcp/{server_id}/oauth-token", stub("mcp.oauth_token_delete"))
	r.Get("/api/apps/{app_id}/mcp/pending-oauth", stub("mcp.pending_oauth"))
	r.Get("/api/apps/{app_id}/payload-schema", stub("apps.payload_schema"))
	r.Get("/api/apps/{app_id}/hooks", stub("apps.hooks"))
	r.Get("/api/apps/{app_id}/channels/health", stub("apps.channels_health"))
}
