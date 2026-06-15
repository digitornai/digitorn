package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

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
	writeJSON(w, http.StatusOK, map[string]any{"pieces": list, "count": len(list)})
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
		entry, ok, err := d.mcpHub.FeaturedByID(r.Context(), req.HubID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "hub_error", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "piece not found in hub catalog: "+req.HubID)
			return
		}
		if entry.HostedURL == nil || *entry.HostedURL == "" {
			writeError(w, http.StatusUnprocessableEntity, "no_bundle", "hub entry has no bundle URL (hosted_url)")
			return
		}

		// Derive piece_name from hub entry when not explicitly provided.
		if pieceName == "" {
			pieceName = entry.ServerID
		}
		if version == "" {
			version = entry.Runtime // hub stores AP version in the runtime field for pieces
		}

		// Download the bundle into the piecesDir.
		if err := pm.DownloadBundle(r.Context(), *entry.HostedURL, pieceName); err != nil {
			writeError(w, http.StatusInternalServerError, "download_failed", err.Error())
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
