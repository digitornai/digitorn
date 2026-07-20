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
	// ----- App-embedded preview UI (unauthenticated static bundle) -----
	// Apps that SHIP a built preview (Excalidraw, scribe…) place it in the
	// install dir at {app}/web/dist/. Served once, SHARED by every session of the
	// app (NOT copied per-workdir). Static, non-secret app UI code — the in-page
	// SDK carries the session context and handles per-session auth. Distinct from
	// the agent-built workdir preview above, which stays untouched.
	r.With(d.panicRecoverer).Get(
		"/api/apps/{app_id}/web-static/*",
		d.serveAppWeb,
	)
	// Template previews : static, non-secret bundle content loaded by direct
	// iframe/img navigation (no JWT possible) — same class as web-static.
	r.With(d.panicRecoverer).Get("/api/apps/{app_id}/templates/{template_id}/preview", d.serveTemplatePreview)
	r.With(d.panicRecoverer).Get("/api/apps/{app_id}/templates/{template_id}/preview/*", d.serveTemplatePreview)
	// Preview file R/W for the embedded-preview SDK — `?t=`-authed (the iframe
	// can't carry the JWT). The read/write half of useSharedDoc; confined to the
	// session workdir. The authenticated /workspace/files routes stay untouched.
	r.With(d.panicRecoverer).Get(
		"/api/apps/{app_id}/sessions/{session_id}/preview/files/*",
		d.getPreviewFile,
	)
	r.With(d.panicRecoverer).Put(
		"/api/apps/{app_id}/sessions/{session_id}/preview/files/*",
		d.putPreviewFile,
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
	// App-agnostic callback for the discovery/DCR flow: a dynamically-registered
	// client is bound to ONE redirect URI, and the state carries app+server, so a
	// single URI serves every app.
	r.With(d.panicRecoverer).Get("/api/oauth/mcp/callback", d.mcpOAuthCallback)

	r.Group(func(r chi.Router) {
		r.Use(d.panicRecoverer) // first: no handler panic ever escapes (jamais crash)
		if d.jwtVerifier != nil {
			r.Use(jwtAuthMiddleware(d.jwtVerifier, d.cfg.Auth.DevMode))
		}
		r.Use(authMiddleware)
		r.Use(d.actAsMiddleware) // trusted on-behalf-of (X-Act-As-User) → effective user

		// Inbound media: upload bytes → content-addressed BlobRef a message can attach.
		// App-scoped (not session) so the blob exists before a per_event session is
		// created. Generic — web/CLI/background channels/voice all use it.
		r.Post("/api/apps/{app_id}/blobs", d.uploadBlob)
		// Outbound media: stream a stored blob by hash (assistant-generated
		// images, tool image outputs). Content-addressed → immutable + cacheable.
		r.Get("/api/apps/{app_id}/blobs/{hash}", d.getBlob)

		// Batch STT for the web composer's voice input ("whisper" mode in
		// voice-input.ts). Audio → llm worker → gateway with the caller's JWT.
		r.Post("/api/transcribe", d.transcribeAudio)

		// ----- Automations : user-scoped window onto the background service -----
		// The bg /ops API is admin-only ; these routes enforce per-user ownership
		// and relay server-side, so the ops token never reaches a client.
		r.Route("/api/automations", func(r chi.Router) {
			r.Get("/schedules", d.listAutomationSchedules)
			r.Post("/schedules", d.createAutomationSchedule)
			r.Post("/schedules/{id}/enable", d.toggleAutomationSchedule(true))
			r.Post("/schedules/{id}/disable", d.toggleAutomationSchedule(false))
			r.Get("/runs", d.listAutomationRuns)
			r.Get("/health", d.automationHealth)
			r.Post("/triggers/{id}/enable", d.toggleAutomationTrigger(true))
			r.Post("/triggers/{id}/disable", d.toggleAutomationTrigger(false))
			r.Post("/jobs/{id}/replay", d.replayAutomationJob)
		})

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
				r.Post("/agents/{agent_id}/cancel", d.cancelAgent)
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

				// GitHub export : link the session workdir to a repo + push.
				r.Get("/github/status", d.githubStatus)
				r.Post("/github/link", d.githubLink)
				r.Post("/github/push", d.githubPush)
				r.Post("/github/clone", d.githubClone)
				r.Post("/github/create", d.githubCreate)
				r.Post("/github/pull", d.githubPull)
				r.Get("/vercel/status", d.vercelStatus)
				r.Get("/vercel/logs", d.vercelLogs)
				r.Post("/vercel/connect", d.vercelConnect)
				r.Post("/vercel/oauth/start", d.vercelOAuthStart)
				r.Post("/vercel/deploy", d.vercelDeploy)
				r.Get("/vercel/env", d.vercelEnvList)
				r.Post("/vercel/env", d.vercelEnvSet)
				r.Delete("/vercel/env/{id}", d.vercelEnvDelete)
				r.Get("/supabase/status", d.supabaseStatus)
				r.Post("/supabase/oauth/start", d.supabaseOAuthStart)
				r.Get("/supabase/projects", d.supabaseProjects)
				r.Post("/supabase/connect", d.supabaseConnect)
				r.Post("/supabase/restore", d.supabaseRestore)
				r.Post("/supabase/create", d.supabaseCreate)
				r.Get("/vercel/domains", d.vercelDomainList)
				r.Post("/vercel/domains", d.vercelDomainAdd)
				r.Delete("/vercel/domains/{domain}", d.vercelDomainRemove)
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
		r.Get("/api/apps/{app_id}/channel-secrets", d.appChannelSecrets)
		r.Get("/api/apps/{app_id}/channel-secret", d.appChannelSecretValue)
		r.Put("/api/auth/background-token", d.setBackgroundToken)
		r.Get("/api/apps/{app_id}/secrets", d.listSecrets)
		r.Get("/api/apps/{app_id}/secrets/{key}", d.getSecret)
		r.Put("/api/apps/{app_id}/secrets", d.setSecrets)
		r.Put("/api/apps/{app_id}/secrets/{key}", d.setSecret)
		r.Delete("/api/apps/{app_id}/secrets/{key}", d.deleteSecret)

		// ----- Per-user default models for new sessions -----
		r.Get("/api/apps/{app_id}/model-defaults", d.getModelDefaults)
		r.Put("/api/apps/{app_id}/model-defaults", d.putModelDefaults)

		// ----- App module settings (schema + values) -----
		r.Get("/api/apps/{app_id}/module-settings", d.appModuleSettings)
		r.Put("/api/apps/{app_id}/modules/{module_id}/config", d.setAppModuleConfig)

		// ----- Credential vault (per-user, encrypted at rest) -----
		r.Get("/api/credentials", d.credentialsList)
		r.Post("/api/credentials", d.credentialsCreate)
		r.Get("/api/credentials/providers", d.credentialsProviders)
		r.Get("/api/credentials/models", d.credentialsModels)
		r.Post("/api/credentials/test", d.credentialsTest)
		r.Get("/api/credentials-grants", d.credentialsGrants)
		r.Post("/api/credentials/copilot/device/start", d.credentialsCopilotStart)
		r.Get("/api/credentials/copilot/device/status", d.credentialsCopilotStatus)
		r.Get("/api/credentials/copilot/models", d.credentialsCopilotModels)
		r.Put("/api/credentials/{id}", d.credentialsUpdate)
		r.Delete("/api/credentials/{id}", d.credentialsDelete)
		r.Post("/api/credentials/{id}/refresh", d.credentialsRefresh)

		// ----- Diagnostics / Status -----
		r.Get("/api/apps/{app_id}/diagnostics", d.diagnostics)
		r.Get("/api/apps/{app_id}/status", d.appStatus)
		r.Get("/api/apps/{app_id}/errors", d.appErrors)
		r.Get("/api/apps/{app_id}/ui-config", d.uiConfig)
		r.Get("/api/apps/{app_id}/deploy-status", d.deployStatus)

		// ----- Daemon stats / observability -----
		r.Get("/api/daemon/stats", d.daemonStats)

		// ----- Activepieces connector hub (pieces module) -----
		r.Route("/api/pieces", func(r chi.Router) {
			r.Get("/", d.piecesList)
			r.Get("/catalog", d.piecesCatalogHub)
			r.Post("/", d.piecesInstall)
			r.Get("/tools", d.piecesTools)
			r.Post("/reload", d.piecesReload)
			r.Put("/{piece_name}", d.piecesUpdateCreds)
			r.Delete("/{piece_name}", d.piecesUninstall)
			r.Get("/{piece_name}/auth-schema", d.piecesAuthSchema)
			r.Get("/{piece_name}/bridge-auth", d.piecesBridgeAuth)
			r.Get("/{piece_name}/status", d.piecesStatus)
			r.Post("/{piece_name}/configure", d.piecesConfigure)
			r.Post("/{piece_name}/test", d.piecesTestAuth)
			r.Post("/{piece_name}/oauth/start", d.piecesOAuthStart)
		})

		// GitHub account connection (workspace export).
		r.Post("/api/github/oauth/start", d.githubOAuthStart)
		// The connected user's repos, for the "open a repo" picker (user-scoped).
		r.Get("/api/github/repos", d.githubRepos)

		// ----- MCP server management : discovery (Phase 1, daemon-level) -----
		// Browse the static catalog + the official MCP registry and inspect what a
		// server needs before installing it into an app.
		r.Route("/api/mcp", func(r chi.Router) {
			// Discovery (Phase 1, read-only).
			r.Get("/catalog", d.mcpCatalogList)
			r.Get("/catalog/{id}", d.mcpCatalogGet)
			r.Get("/search", d.mcpSearch)
			r.Get("/registry/browse", d.mcpRegistryBrowse)
			r.Get("/requirements/{id}", d.mcpRequirements)

			// Managed-server store (Phase 2, per-user CRUD + connectivity test).
			r.Post("/servers", d.mcpInstallServer)
			r.Get("/servers", d.mcpListServers)
			r.Get("/servers/{id}", d.mcpGetServer)
			r.Put("/servers/{id}", d.mcpUpdateServer)
			r.Delete("/servers/{id}", d.mcpDeleteServer)
			r.Post("/servers/{id}/test", d.mcpTestServer)
			r.Post("/servers/{id}/connect", d.mcpConnectServer)
		})

		// ----- App manager (install, list, get, lifecycle, serving) -----
		d.mountAppRoutes(r)

		// ----- Dev-only debug routes (gated by cfg.Auth.DevMode) -----
		if d.cfg.Auth.DevMode {
			r.Post("/api/_dev/invoke", d.devInvoke)
			r.Get("/api/_dev/pieces/catalog", d.piecesCatalogDiag)
		}

		// Dogfooding: drive an owned session's tools through the EXACT agent
		// path (gates + meta-dispatcher + doc-sentinel) from outside the
		// daemon. Real JWT auth + session ownership enforced.
		r.Post("/api/apps/{app_id}/sessions/{session_id}/tools/execute", d.devToolExecute)
		r.Get("/api/apps/{app_id}/tools/surface", d.devToolSurface)

		// ----- Events stream (for background service primitives adapter) -----
		r.Get("/api/events/recent", d.handleEventsRecent)

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
	r.Get("/api/apps/{app_id}/pieces", d.getAppPieces)
	r.Put("/api/apps/{app_id}/pieces", d.setAppPieces)
	r.Put("/api/apps/{app_id}/display-name", d.setAppDisplayName)
	r.Post("/api/apps/{app_id}/reload", d.reloadApp)
	r.Get("/api/apps/{app_id}/check-update", d.checkUpdate)

	// Requirements provisioning (external binaries the app declares, consent-gated).
	r.Get("/api/apps/{app_id}/requirements", d.getRequirements)
	r.Post("/api/apps/{app_id}/requirements/provision", d.provisionRequirements)

	// Read.
	r.Get("/api/apps", d.listApps)
	r.Get("/api/apps/disabled", d.listDisabledApps)
	r.Get("/api/apps/health", d.appsHealth)
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
	r.Post("/api/apps/{app_id}/sessions/{session_id}/workspace/fileop", d.workspaceFileOp)
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
	r.Get("/api/apps/{app_id}/templates", d.listTemplates)
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
