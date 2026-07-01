package server

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/mcpservers"
	"github.com/digitornai/digitorn/internal/modules/mcp"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
)

// managedMCPAppID is the sentinel app id under which per-user managed MCP servers
// run their OAuth flow. The server URL the discovery needs (which the app-keyed
// lookup can't resolve for a per-user server) is passed via mcpoauth.WithServerURL.
const managedMCPAppID = "@mcp-managed"

// piecesAppID is the sentinel app id for per-user piece OAuth tokens.
const piecesAppID = "@pieces"

// MCP server management — daemon-level discovery (Phase 1, read-only). These
// routes let a client browse the static catalog, search/browse the official MCP
// registry, and ask what a server needs (credentials/env/OAuth + a copy-paste
// YAML block) BEFORE installing it into an app. They are stateless: no managed-
// server store, they reuse the same catalog + registry + auth-detection the
// runtime resolver uses, so what a client previews is exactly what would wire up.

// mcpCatalogList — GET /api/mcp/catalog. Prefers the Hub's curated+verified
// catalog (the central source); the static built-in catalog is the offline
// fallback so discovery never goes dark when the Hub is unreachable.
func (d *Daemon) mcpCatalogList(w http.ResponseWriter, r *http.Request) {
	if d.mcpHub != nil {
		if entries, err := d.mcpHub.Featured(r.Context()); err == nil && len(entries) > 0 {
			out := make([]mcp.CatalogInfo, 0, len(entries))
			for _, e := range entries {
				out = append(out, mcp.HubCatalogInfo(e))
			}
			writeJSON(w, http.StatusOK, map[string]any{"servers": out, "count": len(out), "source": "hub"})
			return
		}
	}
	entries := mcp.CatalogList()
	writeJSON(w, http.StatusOK, map[string]any{"servers": entries, "count": len(entries), "source": "static"})
}

// mcpCatalogGet — GET /api/mcp/catalog/{id}. Hub-first, static fallback.
func (d *Daemon) mcpCatalogGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if d.mcpHub != nil {
		if e, ok, err := d.mcpHub.FeaturedByID(r.Context(), id); err == nil && ok {
			writeJSON(w, http.StatusOK, mcp.HubCatalogInfo(e))
			return
		}
	}
	entry, ok := mcp.CatalogGet(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no catalog server "+id)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// mcpSearch — GET /api/mcp/search?q=  (catalog substring + best registry hit)
func (d *Daemon) mcpSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		q = r.URL.Query().Get("query")
	}
	results := mcp.Search(r.Context(), q)
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   q,
		"results": results,
		"count":   len(results),
	})
}

// mcpRegistryBrowse — GET /api/mcp/registry/browse?q=&cursor=&limit=. Prefers
// the Hub's registry (semantic search over a mirrored index); falls back to a
// direct upstream-registry browse when the Hub is unreachable.
func (d *Daemon) mcpRegistryBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	cursor := r.URL.Query().Get("cursor")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if d.mcpHub != nil {
		if servers, next, err := d.mcpHub.RegistryBrowse(r.Context(), q, cursor, limit); err == nil {
			out := make([]mcp.SearchResult, 0, len(servers))
			for _, s := range servers {
				out = append(out, mcp.SearchResult{
					ServerID: s.ServerID, Name: s.Name, Description: s.Description,
					Source: "registry", Runtime: s.Runtime, Package: s.Package, Transport: s.Transport,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"results": out, "count": len(out), "next_cursor": next, "via": "hub"})
			return
		}
	}
	results, next := mcp.RegistryBrowse(r.Context(), q, cursor, limit)
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results), "next_cursor": next, "via": "upstream"})
}

// mcpRequirements — GET /api/mcp/requirements/{id}. Hub-first (its entries carry
// per-field key_descriptions for the connect form); else static catalog/registry.
func (d *Daemon) mcpRequirements(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if d.mcpHub != nil {
		if e, ok, err := d.mcpHub.FeaturedByID(r.Context(), id); err == nil && ok {
			writeJSON(w, http.StatusOK, mcp.HubRequirements(e))
			return
		}
	}
	req, ok := mcp.Requirements(r.Context(), id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no server "+id+" in catalog or registry")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// --- Managed-server store (Phase 2, per-user CRUD) ---------------------------
//
// A user installs a server ONCE (from the catalog, the registry, or a raw spec);
// it lives in their per-user store with secret VALUES sealed at rest. An app
// opts in to a managed server by id (runtime layering — next brick). These
// routes are the management surface the CLI drives.

// managedReady guards the store routes: managed servers carry sealed secrets, so
// they need the process sealer. nil store → 503 (same contract as oauthReady).
func (d *Daemon) managedReady(w http.ResponseWriter) bool {
	if d.managedMCP == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp_servers_unavailable", "managed MCP servers are not configured on this daemon")
		return false
	}
	return true
}

type mcpInstallReq struct {
	ServerID    string            `json:"server_id"`
	From        string            `json:"from"` // catalog | registry | custom (inferred when empty)
	DisplayName string            `json:"display_name"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	URL         string            `json:"url"`
	Env         map[string]string `json:"env"`         // non-secret config
	Credentials map[string]string `json:"credentials"` // shorthands → mapped to env vars (catalog/registry)
	Secrets     map[string]string `json:"secrets"`     // explicit env-var → value (sealed)
	AuthType    string            `json:"auth_type"`
}

// mcpInstallServer — POST /api/mcp/servers
func (d *Daemon) mcpInstallServer(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) {
		return
	}
	var req mcpInstallReq
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.ServerID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "server_id is required")
		return
	}
	userID := userIDOf(r.Context())

	custom := req.From == "custom" || (req.From == "" && (req.Command != "" || req.URL != ""))
	var spec mcpservers.Spec
	switch {
	case custom:
		spec = mcpservers.Spec{
			ServerID: req.ServerID, DisplayName: req.DisplayName, Source: "custom",
			Transport: req.Transport, Command: req.Command, Args: req.Args, URL: req.URL,
			Env: req.Env, Secrets: mergeStrMaps(req.Credentials, req.Secrets), AuthType: req.AuthType,
		}
	case req.From == "hub":
		// The 2-click path: pull the verified config from the Hub, install it
		// into THIS daemon's per-user store. The Hub only provides the config.
		if d.mcpHub == nil {
			writeError(w, http.StatusServiceUnavailable, "hub_unavailable", "the hub client is not configured")
			return
		}
		e, found, err := d.mcpHub.FeaturedByID(r.Context(), req.ServerID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "hub_error", err.Error())
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not_found", "hub has no featured server "+req.ServerID)
			return
		}
		res, ok := mcp.ResolveInstallHub(r.Context(), e, req.Credentials)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "unresolvable", "hub entry "+req.ServerID+" could not be resolved")
			return
		}
		spec = mcpservers.Spec{
			ServerID: req.ServerID, DisplayName: firstNonEmptyStr(req.DisplayName, res.DisplayName), Source: "hub",
			Transport: res.Transport, Command: res.Command, Args: res.Args, URL: res.URL,
			Env: mergeStrMaps(res.Env, req.Env), Secrets: mergeStrMaps(res.Secrets, req.Secrets),
			AuthType: firstNonEmptyStr(req.AuthType, res.AuthType), Package: res.Package,
		}
	default:
		res, ok := mcp.ResolveInstall(r.Context(), req.ServerID, req.Credentials)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no server "+req.ServerID+" in catalog or registry (pass command/url + from=custom for a raw spec)")
			return
		}
		spec = mcpservers.Spec{
			ServerID: req.ServerID, DisplayName: firstNonEmptyStr(req.DisplayName, res.DisplayName), Source: res.Source,
			Transport: res.Transport, Command: res.Command, Args: res.Args, URL: res.URL,
			Env: mergeStrMaps(res.Env, req.Env), Secrets: mergeStrMaps(res.Secrets, req.Secrets),
			AuthType: firstNonEmptyStr(req.AuthType, res.AuthType), Package: res.Package,
		}
	}

	server, err := d.managedMCP.Install(r.Context(), userID, spec)
	if err != nil {
		d.writeManagedErr(w, err)
		return
	}
	// Tell the client whether a connect step is still needed (the 2nd "click"):
	// oauth2 servers need a browser authorization; everything else is ready.
	resp := map[string]any{"server": server, "requires_connect": server.AuthType == "oauth2"}
	if server.AuthType == "oauth2" {
		resp["connect_url"] = "/api/mcp/servers/" + server.ServerID + "/connect"
	}
	writeJSON(w, http.StatusCreated, resp)
}

// mcpListServers — GET /api/mcp/servers
func (d *Daemon) mcpListServers(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) {
		return
	}
	servers, err := d.managedMCP.List(r.Context(), userIDOf(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers, "count": len(servers)})
}

// mcpGetServer — GET /api/mcp/servers/{id}
func (d *Daemon) mcpGetServer(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) {
		return
	}
	server, found, err := d.managedMCP.Get(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no managed server "+chi.URLParam(r, "id"))
		return
	}
	writeJSON(w, http.StatusOK, server)
}

type mcpUpdateReq struct {
	DisplayName *string            `json:"display_name"`
	Transport   *string            `json:"transport"`
	Command     *string            `json:"command"`
	Args        *[]string          `json:"args"`
	URL         *string            `json:"url"`
	Env         *map[string]string `json:"env"`
	Secrets     *map[string]string `json:"secrets"`
	AuthType    *string            `json:"auth_type"`
}

// mcpUpdateServer — PUT /api/mcp/servers/{id}
func (d *Daemon) mcpUpdateServer(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) {
		return
	}
	var req mcpUpdateReq
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	patch := mcpservers.Patch{
		DisplayName: req.DisplayName, Transport: req.Transport, Command: req.Command,
		Args: req.Args, URL: req.URL, Env: req.Env, Secrets: req.Secrets, AuthType: req.AuthType,
	}
	server, err := d.managedMCP.Update(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id"), patch)
	if err != nil {
		d.writeManagedErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, server)
}

// mcpDeleteServer — DELETE /api/mcp/servers/{id}
func (d *Daemon) mcpDeleteServer(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) {
		return
	}
	if err := d.managedMCP.Delete(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id")); err != nil {
		d.writeManagedErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mcpTestServer — POST /api/mcp/servers/{id}/test : dial the stored server +
// list its tools (a connectivity check), without touching the live pool.
func (d *Daemon) mcpTestServer(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) {
		return
	}
	view, secrets, found, err := d.managedMCP.Reveal(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reveal_failed", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no managed server "+chi.URLParam(r, "id"))
		return
	}
	// Transport-aware credential placement: a stdio server reads secrets from env
	// vars (key = var name); a remote server reads them from request headers
	// (key = header name, e.g. Authorization). Hosted servers ride this path too —
	// their digitorn_provided values are stored as secrets → headers here.
	probe := mcp.ProbeInput{
		Transport: view.Transport,
		Command:   view.Command,
		Args:      view.Args,
		URL:       view.URL,
		Env:       view.Env,
	}
	if view.Transport == "stdio" || view.Transport == "" {
		probe.Env = mergeStrMaps(view.Env, secrets)
	} else {
		probe.Headers = secrets
		// An oauth2 remote server carries no stored secret — its bearer token comes
		// from the OAuth store, injected here so connect→test closes the loop.
		if view.AuthType == "oauth2" {
			probe.Headers = mergeStrMaps(secrets, d.managedOAuthHeaders(r.Context(), view.URL))
		}
	}
	result := mcp.ProbeConfig(r.Context(), probe)
	status := http.StatusOK
	if !result.OK {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, result)
}

// mcpConnectServer — POST /api/mcp/servers/{id}/connect : for an oauth2 managed
// server, mint a browser authorize URL (generic discovery + DCR from the server
// URL). The token lands keyed by the discovered provider; the connection then
// uses it (test-dial injects it; full app-runtime use rides the layering brick).
func (d *Daemon) mcpConnectServer(w http.ResponseWriter, r *http.Request) {
	if !d.managedReady(w) || !d.oauthReady(w) {
		return
	}
	userID := userIDOf(r.Context())
	id := chi.URLParam(r, "id")
	view, found, err := d.managedMCP.Get(r.Context(), userID, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no managed server "+id)
		return
	}
	if view.AuthType != "oauth2" {
		writeError(w, http.StatusBadRequest, "not_oauth2", "server "+id+" is not an oauth2 server (auth_type="+view.AuthType+")")
		return
	}
	if view.URL == "" {
		writeError(w, http.StatusBadRequest, "no_url", "oauth2 server "+id+" has no URL to discover an authorization server from")
		return
	}
	cfg := &schema.MCPAuthConfig{Type: "oauth2"}
	ctx := mcpoauth.WithServerURL(r.Context(), view.URL)
	authURL, state, err := d.mcpOAuth.Authorize(ctx, cfg, userID, managedMCPAppID, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "authorize_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL, "state": state, "server_id": id,
		"provider": d.mcpOAuth.ProviderKeyResolved(ctx, cfg, managedMCPAppID, id),
	})
}

// managedOAuthHeaders returns the Authorization header for an oauth2 managed
// server when a token exists for its provider, else nil (server not connected).
func (d *Daemon) managedOAuthHeaders(ctx context.Context, serverURL string) map[string]string {
	if d.mcpOAuth == nil || serverURL == "" {
		return nil
	}
	cfg := &schema.MCPAuthConfig{Type: "oauth2"}
	ctx = mcpoauth.WithServerURL(ctx, serverURL)
	ac := d.mcpOAuth.ListingAuthContext(ctx, cfg, managedMCPAppID, "")
	if ac == nil || ac.Token == "" {
		return nil
	}
	scheme := ac.TokenType
	if scheme == "" {
		scheme = "Bearer"
	}
	return map[string]string{"Authorization": scheme + " " + ac.Token}
}

// writeManagedErr maps store errors to HTTP status codes.
func (d *Daemon) writeManagedErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcpservers.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, mcpservers.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, mcpservers.ErrInvalidID), errors.Is(err, mcpservers.ErrInvalidSpec):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
	}
}

func firstNonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// mergeStrMaps returns base overlaid by over (over wins). nil-safe.
func mergeStrMaps(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}
