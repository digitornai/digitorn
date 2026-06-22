package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/mcphub"
	"github.com/mbathepaul/digitorn/internal/modules/pieces"
)

// piecesModule returns the daemon's in-process pieces module, or nil when it
// is either absent (binary not deployed yet) or workerised.
func (d *Daemon) piecesModule() *pieces.Module {
	mod, ok := d.bus.Get("pieces")
	if !ok {
		return nil
	}
	pm, _ := mod.(*pieces.Module)
	return pm
}

// GET /api/pieces — list installed pieces for the caller.
func (d *Daemon) piecesList(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	store := pm.PiecesStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store not initialised")
		return
	}
	userID := userIDOf(r.Context())
	list, err := store.List(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}

	// Enrich with action/trigger counts and configured status from the bridge.
	bridge := pm.Bridge()
	type enriched struct {
		UserID        string `json:"UserID"`
		PieceName     string `json:"PieceName"`
		Version       string `json:"Version"`
		AuthType      string `json:"AuthType"`
		Enabled       bool   `json:"Enabled"`
		ActionCount   int    `json:"actionCount"`
		TriggerCount  int    `json:"triggerCount"`
		Configured    bool   `json:"configured"`
	}
	out := make([]enriched, 0, len(list))
	for _, p := range list {
		e := enriched{
			UserID:    p.UserID,
			PieceName: p.PieceName,
			Version:   p.Version,
			AuthType:  p.AuthType,
			Enabled:   p.Enabled,
			Configured: len(p.SecretKeys) > 0,
		}
		if bridge != nil {
			if status, err2 := bridge.GetPieceStatus(p.PieceName); err2 == nil {
				if v, ok := status["actionCount"].(float64); ok {
					e.ActionCount = int(v)
				}
				if v, ok := status["triggerCount"].(float64); ok {
					e.TriggerCount = int(v)
				}
			}
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pieces": out, "count": len(out)})
}

// POST /api/pieces — install a piece for the caller.
//
// From the hub (recommended):
//
//	{ "hub_id": "github", "auth_type": "secret_text", "credentials": {"value":"ghp_..."} }
//
// Manual (local bundle already in piecesDir):
//
//	{ "piece_name": "github", "version": "0.8.3", "auth_type": "secret_text", "credentials": {...} }
func (d *Daemon) piecesInstall(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	store := pm.PiecesStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store not initialised")
		return
	}

	var req struct {
		// Hub flow: install from the hub's MCP featured catalog.
		HubID string `json:"hub_id"`
		// Manual flow: piece bundle is already in piecesDir.
		PieceName string `json:"piece_name"`
		// Common fields.
		Version     string            `json:"version"`
		AuthType    string            `json:"auth_type"`
		Credentials map[string]string `json:"credentials"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	pieceName := req.PieceName
	version := req.Version
	authType := req.AuthType

	// Hub flow: resolve piece metadata + download bundle.
	if req.HubID != "" {
		if d.mcpHub == nil {
			writeError(w, http.StatusServiceUnavailable, "hub_unavailable", "hub client not configured")
			return
		}
		entry, ok, err := d.mcpHub.PiecesGet(r.Context(), req.HubID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "hub_error", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "piece not found in hub catalog: "+req.HubID)
			return
		}

		// Derive piece_name from hub entry when not explicitly provided.
		if pieceName == "" {
			pieceName = entry.ServerID
		}

		// Download the bundle from the hub into the piecesDir.
		bundleURL := d.mcpHub.PiecesBundleURL(entry.ServerID)
		if err := pm.DownloadBundle(r.Context(), bundleURL, pieceName); err != nil {
			code := http.StatusInternalServerError
			errCode := "download_failed"
			if strings.Contains(err.Error(), "HTTP 404") {
				code = http.StatusNotFound
				errCode = "bundle_not_found"
			}
			writeError(w, code, errCode, err.Error())
			return
		}
	}

	if pieceName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "piece_name or hub_id is required")
		return
	}
	if authType == "" {
		authType = "none"
	}

	userID := userIDOf(r.Context())
	if err := store.Install(r.Context(), userID, pieceName, version, authType, req.Credentials); err != nil {
		writeError(w, http.StatusInternalServerError, "install_failed", err.Error())
		return
	}

	go pm.ReloadBridge(context.Background()) //nolint:errcheck
	d.piecesCatalog.invalidate("") // invalidate all cached tool lists

	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "piece_name": pieceName})
}

// PUT /api/pieces/{piece_name} — update credentials for an installed piece.
func (d *Daemon) piecesUpdateCreds(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	store := pm.PiecesStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store not initialised")
		return
	}

	pieceName := chi.URLParam(r, "piece_name")
	var req struct {
		Credentials map[string]string `json:"credentials"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	userID := userIDOf(r.Context())
	if err := store.Update(r.Context(), userID, pieceName, req.Credentials); err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /api/pieces/{piece_name} — uninstall a piece.
func (d *Daemon) piecesUninstall(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	store := pm.PiecesStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store not initialised")
		return
	}

	pieceName := chi.URLParam(r, "piece_name")
	userID := userIDOf(r.Context())
	if err := store.Delete(r.Context(), userID, pieceName); err != nil {
		writeError(w, http.StatusInternalServerError, "uninstall_failed", err.Error())
		return
	}
	go pm.ReloadBridge(context.Background()) //nolint:errcheck
	d.piecesCatalog.invalidate("") // invalidate all cached tool lists
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/pieces/tools — list all tools the bridge currently exposes.
func (d *Daemon) piecesTools(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	specs := pm.LiveTools(r.Context())
	out := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		b, _ := json.Marshal(s)
		var m map[string]any
		json.Unmarshal(b, &m) //nolint:errcheck
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out, "count": len(out)})
}

// POST /api/pieces/reload — restart the bridge (admin/dev use).
func (d *Daemon) piecesReload(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	if err := pm.ReloadBridge(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "reload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/pieces/catalog — list pieces available from the hub.
func (d *Daemon) piecesCatalogHub(w http.ResponseWriter, r *http.Request) {
	var pieces []mcphub.PieceEntry

	// Fetch from hub if available.
	if d.mcpHub != nil {
		hubPieces, err := d.mcpHub.PiecesList(r.Context())
		if err == nil {
			pieces = append(pieces, hubPieces...)
		}
	}

	// Also include locally installed pieces not in the hub catalog.
	pm := d.piecesModule()
	if pm != nil {
		store := pm.PiecesStore()
		userID := userIDOf(r.Context())
		installed, err := store.List(r.Context(), userID)
		if err == nil {
			hubIDs := make(map[string]bool, len(pieces))
			for _, p := range pieces {
				hubIDs[p.ServerID] = true
			}
			for _, inst := range installed {
				if hubIDs[inst.PieceName] {
					continue
				}
				authType := "none"
				displayName := inst.PieceName
				if bridge := pm.Bridge(); bridge != nil {
					if status, err := bridge.GetPieceStatus(inst.PieceName); err == nil {
						if v, ok := status["displayName"].(string); ok && v != "" {
							displayName = v
						}
						if v, ok := status["authType"].(string); ok {
							authType = v
						}
					}
				}
				pieces = append(pieces, mcphub.PieceEntry{
					ServerID:    inst.PieceName,
					DisplayName: displayName,
					AuthType:    authType,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"pieces": pieces, "count": len(pieces)})
}

// GET /api/pieces/_dev/pieces/catalog — diagnostic: what the pieces catalog returns.
func (d *Daemon) piecesCatalogDiag(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("app")
	if appID == "" {
		appID = "chat"
	}
	actions := d.piecesCatalog.forApp(appID)
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id": appID,
		"count":  len(actions),
		"actions": actions,
	})
}

// ── Auth schema + configuration endpoints ──────────────────────────────

// GET /api/pieces/{piece_name}/auth-schema — returns the auth requirements for a piece.
func (d *Daemon) piecesAuthSchema(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}

	pieceName := chi.URLParam(r, "piece_name")

	// Get auth schema from the bridge trigger server
	schema, err := pm.Bridge().GetPieceAuth(pieceName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bridge_error", err.Error())
		return
	}

	// Enrich with install status from store
	userID := userIDOf(r.Context())
	if pm.PiecesStore() != nil {
		_, installed, _ := pm.PiecesStore().Get(r.Context(), userID, pieceName)
		schema["installed"] = installed
	}

	writeJSON(w, http.StatusOK, schema)
}

// GET /api/pieces/{piece_name}/status — returns piece status including config.
func (d *Daemon) piecesStatus(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}

	pieceName := chi.URLParam(r, "piece_name")

	// Get status from bridge
	status, err := pm.Bridge().GetPieceStatus(pieceName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bridge_error", err.Error())
		return
	}

	// Enrich with store info
	userID := userIDOf(r.Context())
	if pm.PiecesStore() != nil {
		view, installed, err := pm.PiecesStore().Get(r.Context(), userID, pieceName)
		if err == nil && view != nil {
			status["configured"] = installed
			status["auth_type"] = view.AuthType
			status["secret_keys"] = view.SecretKeys
			status["enabled"] = view.Enabled
		}
	}

	writeJSON(w, http.StatusOK, status)
}

// POST /api/pieces/{piece_name}/configure — store credentials for a piece.
func (d *Daemon) piecesConfigure(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	store := pm.PiecesStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store not initialised")
		return
	}

	pieceName := chi.URLParam(r, "piece_name")
	var req struct {
		AuthType    string            `json:"auth_type"`
		Credentials map[string]string `json:"credentials"`
		Version     string            `json:"version"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if req.AuthType == "" {
		req.AuthType = "none"
	}

	userID := userIDOf(r.Context())
	if err := store.Install(r.Context(), userID, pieceName, req.Version, req.AuthType, req.Credentials); err != nil {
		writeError(w, http.StatusInternalServerError, "configure_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "piece_name": pieceName, "auth_type": req.AuthType})
}

// POST /api/pieces/{piece_name}/test — test credentials for a piece.
func (d *Daemon) piecesTestAuth(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}

	var req struct {
		AuthType    string            `json:"auth_type"`
		Credentials map[string]string `json:"credentials"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Build the auth wire object
	wire := buildAuthWire(req.AuthType, req.Credentials)

	// Test by calling a simple action or just validating the auth format
	// For now, we validate the auth type is supported
	if wire == nil {
		writeError(w, http.StatusBadRequest, "invalid_auth", "unsupported auth type: "+req.AuthType)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"auth_type": req.AuthType,
		"message":   "Auth format valid. Credentials will be tested on first tool call.",
	})
}

func buildAuthWire(authType string, creds map[string]string) *pieces.APAuthWire {
	switch authType {
	case "secret_text":
		return &pieces.APAuthWire{Type: "secret_text", Value: creds["value"]}
	case "basic":
		return &pieces.APAuthWire{Type: "basic", Username: creds["username"], Password: creds["password"]}
	case "oauth2":
		return &pieces.APAuthWire{
			Type:         "oauth2",
			AccessToken:  creds["access_token"],
			TokenType:    creds["token_type"],
			RefreshToken: creds["refresh_token"],
			Scope:        creds["scope"],
		}
	case "custom":
		return &pieces.APAuthWire{Type: "custom", Fields: creds}
	case "none":
		return &pieces.APAuthWire{Type: "none"}
	default:
		return nil
	}
}

// POST /api/pieces/{piece_name}/oauth/start — start OAuth2 flow for a piece.
func (d *Daemon) piecesOAuthStart(w http.ResponseWriter, r *http.Request) {
	if d.mcpOAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "OAuth is not configured on this daemon.")
		return
	}
	pm := d.piecesModule()
	if pm == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces module is not running")
		return
	}
	bridge := pm.Bridge()
	if bridge == nil {
		writeError(w, http.StatusServiceUnavailable, "bridge_unavailable", "pieces bridge is not running")
		return
	}

	pieceName := chi.URLParam(r, "piece_name")
	userID := userIDOf(r.Context())

	// Parse request body for optional client credentials.
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	_ = readJSONLenient(r, &req)

	// Get the auth schema from the bridge to find the OAuth2 option.
	authSchema, err := bridge.GetPieceAuth(pieceName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bridge_error", err.Error())
		return
	}

	// Find the OAuth2 option from the auth schema.
	options, _ := authSchema["options"].([]any)
	var oauthOpt map[string]any
	if len(options) == 0 {
		if authSchema["type"] == "OAUTH2" {
			oauthOpt = authSchema
		}
	} else {
		for _, opt := range options {
			if m, ok := opt.(map[string]any); ok && m["type"] == "OAUTH2" {
				oauthOpt = m
				break
			}
		}
	}
	if oauthOpt == nil {
		writeError(w, http.StatusBadRequest, "not_oauth2", "This piece does not support OAuth2.")
		return
	}

	oauth, _ := oauthOpt["oauth"].(map[string]any)
	authURL, _ := oauth["authUrl"].(string)
	tokenURL, _ := oauth["tokenUrl"].(string)
	scopeArr, _ := oauth["scope"].([]any)
	var scopes []string
	for _, s := range scopeArr {
		if str, ok := s.(string); ok {
			scopes = append(scopes, str)
		}
	}

	// Build the auth config from the piece's OAuth schema.
	cfg := &schema.MCPAuthConfig{
		Type:         "oauth2",
		Provider:     "custom",
		AuthorizeURL: authURL,
		TokenURL:     tokenURL,
		Scopes:       scopes,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
	}

	// Start the OAuth flow using the existing mcpoauth service.
	authURL, state, err := d.mcpOAuth.AuthorizeForPiece(r.Context(), cfg, userID, piecesAppID, pieceName, req.ClientID, req.ClientSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oauth_start_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL,
		"state":    state,
		"piece":    pieceName,
	})
}
