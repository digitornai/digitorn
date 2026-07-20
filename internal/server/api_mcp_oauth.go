package server

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
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
		// Browsers sometimes fire the callback twice; the state is single-use, so
		// the duplicate lands here AFTER the first request already succeeded.
		// Show success for a recently-completed state instead of a scary error.
		if recentOAuthCompletions.done(state) {
			writeOAuthHTML(w, http.StatusOK, "Authorization complete. You can close this window.")
			return
		}
		writeOAuthHTML(w, http.StatusBadRequest, "This authorization link is invalid or has expired. Please try again.")
		return
	}
	if p.AppID == vercelOAuthAppID {
		if err := d.vercelCompleteOAuth(r.Context(), p.UserID, code, p.RedirectURI); err != nil {
			writeOAuthHTML(w, http.StatusBadGateway, "Vercel rejected the authorization. Please try again.")
			return
		}
		recentOAuthCompletions.mark(state)
		writeOAuthHTML(w, http.StatusOK, "Vercel connected. You can close this window.")
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
		recentOAuthCompletions.mark(state)
		writeOAuthHTML(w, http.StatusOK, "Authorization complete. You can close this window.")
		return
	}
	// Per-user piece OAuth: exchange the code and store the token in the pieces store.
	if p.AppID == piecesAppID {
		pieceName := p.ServerID
		// Get the auth schema from the bridge to find the provider's OAuth endpoints.
		pm := d.piecesModule()
		if pm == nil {
			writeOAuthHTML(w, http.StatusServiceUnavailable, "Pieces module is not running.")
			return
		}
		bridge := pm.Bridge()
		if bridge == nil {
			writeOAuthHTML(w, http.StatusServiceUnavailable, "Pieces bridge is not running.")
			return
		}
		authSchema, err := bridge.GetPieceAuth(pieceName)
		if err != nil {
			writeOAuthHTML(w, http.StatusBadGateway, "Could not retrieve piece auth schema.")
			return
		}
		// Find the OAuth2 option from the auth schema.
		options, _ := authSchema["options"].([]any)
		var oauthOpt map[string]any
		if len(options) == 0 {
			// Single auth option
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
			writeOAuthHTML(w, http.StatusBadRequest, "This piece does not support OAuth2.")
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
		// Build the auth config from the piece's OAuth schema, using client credentials from state.
		// PKCE off: confidential client, and it must match the authorize step.
		clientID, clientSecret := p.ClientID, p.ClientSecret
		if clientID == "" && d.mcpHub != nil {
			// State predates client-cred persistence (or was stored without them):
			// fall back to the hub's system OAuth app.
			if sys, serr := d.mcpHub.PiecesSystemConfig(r.Context(), pieceName); serr == nil && sys != nil {
				if v, ok := sys.DigitornProvided["oauth_client_id"].(string); ok {
					clientID = v
				}
				if v, ok := sys.DigitornProvided["oauth_client_secret"].(string); ok {
					clientSecret = v
				}
			}
		}
		pkceOff := false
		cfg := &schema.MCPAuthConfig{
			Type:         "oauth2",
			Provider:     "custom",
			AuthorizeURL: authURL,
			TokenURL:     tokenURL,
			Scopes:       scopes,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURI:  p.RedirectURI,
			PKCE:         &pkceOff,
		}
		tok, err := d.mcpOAuth.ExchangeForPiece(r.Context(), cfg, p, code)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pieces oauth: exchange failed for %s: %v\n", pieceName, err)
			writeOAuthHTML(w, http.StatusBadGateway, "The provider rejected the authorization. Please try again.")
			return
		}
		if serr := pm.PiecesStore().UpsertOAuth(r.Context(), p.UserID, pieceName, tok.AccessToken, tok.RefreshToken, tok.TokenType, tok.ExpiresAt, tok.Scope, tokenURL, clientID, clientSecret); serr != nil {
			writeOAuthHTML(w, http.StatusInternalServerError, "Could not store the connection. Please try again.")
			return
		}
		recentOAuthCompletions.mark(state)
		writeOAuthHTML(w, http.StatusOK, "Authorization complete. You can close this window.")
		return
	}
	// Workspace direct-provider OAuth (the GitHub button): started via
	// AuthorizeForPiece with a sentinel app and its own client creds. There's no
	// app MCP-server config for it, so exchange with the state's creds and store
	// the token under the provider — githubToken/GetToken(user, provider) read it.
	if p.AppID == githubWorkspaceAppID {
		cfg := &schema.MCPAuthConfig{
			Type:         "oauth2",
			Provider:     p.Provider,
			ClientID:     p.ClientID,
			ClientSecret: p.ClientSecret,
		}
		tok, xerr := d.mcpOAuth.ExchangeForPiece(r.Context(), cfg, p, code)
		if xerr != nil {
			writeOAuthHTML(w, http.StatusBadGateway, "The provider rejected the authorization. Please try again.")
			return
		}
		if serr := d.mcpOAuth.SetManual(r.Context(), p.UserID, p.Provider, tok); serr != nil {
			writeOAuthHTML(w, http.StatusInternalServerError, "Could not store the connection. Please try again.")
			return
		}
		recentOAuthCompletions.mark(state)
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
	recentOAuthCompletions.mark(state)
	writeOAuthHTML(w, http.StatusOK, "Authorization complete. You can close this window.")
}

// recentOAuthCompletions remembers states whose callback already succeeded so
// a duplicate browser hit (states are single-use) shows success, not an error.
var recentOAuthCompletions = &completedStates{seen: map[string]time.Time{}}

type completedStates struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (c *completedStates) mark(state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for s, t := range c.seen {
		if now.Sub(t) > 10*time.Minute {
			delete(c.seen, s)
		}
	}
	c.seen[state] = now
}

func (c *completedStates) done(state string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.seen[state]
	return ok && time.Since(t) <= 10*time.Minute
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
