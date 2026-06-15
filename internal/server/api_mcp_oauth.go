package server

import (
	"html"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/server/mcpoauth"
)

// mcpServers returns the app's normalized MCP server configs (nil if the app
// declares no mcp module).
func (d *Daemon) mcpServers(appID string) map[string]schema.MCPServerConfig {
	if d.mcpCatalog == nil {
		return nil
	}
	cfg := d.mcpCatalog.appMCPConfig(appID)
	if cfg == nil {
		return nil
	}
	raw, ok := cfg["servers"]
	if !ok {
		return nil
	}
	servers, _ := schema.NormalizeServers(raw)
	return servers
}

// mcpServerAuth returns the oauth2 auth block for one server, or (nil, false)
// when the server is unknown or not oauth2-authenticated.
func (d *Daemon) mcpServerAuth(appID, serverID string) (*schema.MCPAuthConfig, bool) {
	sc, ok := d.mcpServers(appID)[serverID]
	if !ok || sc.Auth == nil || sc.Auth.Type != "oauth2" {
		return nil, false
	}
	return sc.Auth, true
}

// mcpServerAuthLookup adapts mcpServerAuth to mcpoauth.ServerAuthLookup (returns
// nil for unknown / non-oauth2 servers). Wired into the resolver at boot.
func (d *Daemon) mcpServerAuthLookup(appID, serverID string) *schema.MCPAuthConfig {
	cfg, _ := d.mcpServerAuth(appID, serverID)
	return cfg
}

// mcpServerURLLookup returns a server's remote URL (empty for stdio / unknown).
// Wired into the OAuth service so it can discover an authorization server from a
// server declared by URL with no explicit endpoints/client.
func (d *Daemon) mcpServerURLLookup(appID, serverID string) string {
	return d.mcpServers(appID)[serverID].URL
}

func (d *Daemon) oauthReady(w http.ResponseWriter) bool {
	if d.mcpOAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "mcp oauth is not configured on this daemon")
		return false
	}
	return true
}

// mcpOAuthAuthorize mints an authorization URL for (caller, server) and persists
// the state→user binding. GET …/oauth/authorize?server_id=…
func (d *Daemon) mcpOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !d.oauthReady(w) {
		return
	}
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	serverID := r.URL.Query().Get("server_id")
	if serverID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "server_id is required")
		return
	}
	cfg, ok := d.mcpServerAuth(appID, serverID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "server is not oauth2-authenticated")
		return
	}
	authURL, state, err := d.mcpOAuth.Authorize(r.Context(), cfg, userID, appID, serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "authorize_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url":  authURL,
		"state":     state,
		"provider":  d.mcpOAuth.ProviderKeyResolved(r.Context(), cfg, appID, serverID),
		"server_id": serverID,
	})
}

// mcpOAuthCallback finishes the flow: it is hit by the provider's browser
// redirect (no JWT), so it authenticates purely via the state→user binding.
// GET …/oauth/callback?code=…&state=…  (registered OUTSIDE the auth group)
func (d *Daemon) mcpOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if d.mcpOAuth == nil {
		writeOAuthHTML(w, http.StatusServiceUnavailable, "OAuth is not configured on this daemon.")
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		writeOAuthHTML(w, http.StatusBadRequest, "Missing code or state.")
		return
	}
	p, err := d.mcpOAuth.TakeState(r.Context(), state)
	if err != nil {
		writeOAuthHTML(w, http.StatusInternalServerError, "Could not validate the request.")
		return
	}
	if p == nil {
		writeOAuthHTML(w, http.StatusBadRequest, "This authorization link is invalid or has expired. Please try again.")
		return
	}
	// Per-user managed server: rebuild the oauth2 block + the server URL the
	// discovery needs from the managed store (the app-config lookup can't see it).
	if p.AppID == managedMCPAppID {
		if d.managedMCP == nil {
			writeOAuthHTML(w, http.StatusServiceUnavailable, "Managed MCP servers are not configured.")
			return
		}
		view, found, gerr := d.managedMCP.Get(r.Context(), p.UserID, p.ServerID)
		if gerr != nil || !found || view.URL == "" {
			writeOAuthHTML(w, http.StatusBadRequest, "The server is no longer configured for OAuth.")
			return
		}
		ctx := mcpoauth.WithServerURL(r.Context(), view.URL)
		if err := d.mcpOAuth.Exchange(ctx, &schema.MCPAuthConfig{Type: "oauth2"}, p, code); err != nil {
			writeOAuthHTML(w, http.StatusBadGateway, "The provider rejected the authorization. Please try again.")
			return
		}
		writeOAuthHTML(w, http.StatusOK, "Authorization complete. You can close this window.")
		return
	}
	cfg, ok := d.mcpServerAuth(p.AppID, p.ServerID)
	if !ok {
		writeOAuthHTML(w, http.StatusBadRequest, "The server is no longer configured for OAuth.")
		return
	}
	if err := d.mcpOAuth.Exchange(r.Context(), cfg, p, code); err != nil {
		writeOAuthHTML(w, http.StatusBadGateway, "The provider rejected the authorization. Please try again.")
		return
	}
	d.mcpCatalog.invalidate(p.AppID)
	writeOAuthHTML(w, http.StatusOK, "Authorization complete. You can close this window.")
}

// mcpOAuthTokenSet stores a token the client obtained out-of-band.
// POST …/mcp/{server_id}/oauth-token
func (d *Daemon) mcpOAuthTokenSet(w http.ResponseWriter, r *http.Request) {
	if !d.oauthReady(w) {
		return
	}
	appID := chi.URLParam(r, "app_id")
	serverID := chi.URLParam(r, "server_id")
	userID := userIDOf(r.Context())
	cfg, ok := d.mcpServerAuth(appID, serverID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "server is not oauth2-authenticated")
		return
	}
	var req struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresAt    int64  `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.AccessToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "access_token is required")
		return
	}
	tok := &mcpoauth.Token{
		AccessToken:  req.AccessToken,
		RefreshToken: req.RefreshToken,
		TokenType:    req.TokenType,
		ExpiresAt:    req.ExpiresAt,
		Scope:        req.Scope,
	}
	if tok.ExpiresAt == 0 && req.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().UTC().Unix() + req.ExpiresIn
	}
	provider := d.mcpOAuth.ProviderKeyResolved(r.Context(), cfg, appID, serverID)
	if err := d.mcpOAuth.SetManual(r.Context(), userID, provider, tok); err != nil {
		writeError(w, http.StatusInternalServerError, "token_set_failed", err.Error())
		return
	}
	d.mcpCatalog.invalidate(appID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mcpOAuthTokenRevoke removes the caller's token for a server's provider (scoped).
// DELETE …/mcp/{server_id}/oauth-token
func (d *Daemon) mcpOAuthTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !d.oauthReady(w) {
		return
	}
	appID := chi.URLParam(r, "app_id")
	serverID := chi.URLParam(r, "server_id")
	userID := userIDOf(r.Context())
	cfg, ok := d.mcpServerAuth(appID, serverID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "server is not oauth2-authenticated")
		return
	}
	if err := d.mcpOAuth.Revoke(r.Context(), cfg, userID, appID, serverID); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke_failed", err.Error())
		return
	}
	d.mcpCatalog.invalidate(appID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mcpPendingOAuth lists the caller's servers that still need authorization. It
// returns ONLY non-secret fields (no client_id/secret/redirect_uri).
// GET …/mcp/pending-oauth
func (d *Daemon) mcpPendingOAuth(w http.ResponseWriter, r *http.Request) {
	if !d.oauthReady(w) {
		return
	}
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	pending := make([]map[string]any, 0)
	for serverID, sc := range d.mcpServers(appID) {
		if sc.Auth == nil || sc.Auth.Type != "oauth2" {
			continue
		}
		provider := d.mcpOAuth.ProviderKeyResolved(r.Context(), sc.Auth, appID, serverID)
		if d.mcpOAuth.HasValidToken(r.Context(), userID, provider) {
			continue
		}
		entry := map[string]any{
			"server_id":      serverID,
			"provider":       provider,
			"requires_oauth": true,
		}
		if authURL, _, err := d.mcpOAuth.Authorize(r.Context(), sc.Auth, userID, appID, serverID); err == nil {
			entry["auth_url"] = authURL
		}
		pending = append(pending, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending": pending,
		"count":   len(pending),
	})
}

func writeOAuthHTML(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8">` +
		`<title>Digitorn — Authorization</title></head>` +
		`<body style="font-family:system-ui,sans-serif;text-align:center;padding:3rem">` +
		`<p>` + html.EscapeString(message) + `</p>` +
		`<script>setTimeout(function(){try{window.close()}catch(e){}},1500)</script>` +
		`</body></html>`))
}
