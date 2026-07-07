package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/credentials"
)

// Per-user credential vault (settings plane). These handlers touch the DB +
// cipher only on explicit user actions — never on an agent's hot path. Every
// row is scoped to the caller's user id; secrets are sealed at rest and never
// returned (the read API exposes only masked previews). Scope is always
// per-user: there is no app/system scope and no grants.

func (d *Daemon) credsReady(w http.ResponseWriter) bool {
	if d.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable",
			"credential vault unavailable (server key missing)")
		return false
	}
	return true
}

func (d *Daemon) credentialsList(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	rows, err := d.creds.List(r.Context(), userIDOf(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": rows, "count": len(rows)})
}

type credCreateRequest struct {
	ProviderName string            `json:"provider_name"`
	ProviderType string            `json:"provider_type"`
	Label        string            `json:"label"`
	Name         string            `json:"name"`
	Fields       map[string]string `json:"fields"`
}

func (d *Daemon) credentialsCreate(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	var req credCreateRequest
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.ProviderName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider_name is required")
		return
	}
	if len(req.Fields) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "fields is required")
		return
	}
	view, err := d.creds.Create(r.Context(), userIDOf(r.Context()), credentials.CreateInput{
		ProviderName: req.ProviderName,
		ProviderType: req.ProviderType,
		Label:        req.Label,
		Name:         req.Name,
		Fields:       req.Fields,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	d.credResolver.Invalidate(userIDOf(r.Context()))
	writeJSON(w, http.StatusOK, view)
}

type credUpdateRequest struct {
	Label  *string           `json:"label"`
	Fields map[string]string `json:"fields"`
}

func (d *Daemon) credentialsUpdate(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	var req credUpdateRequest
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	view, err := d.creds.Update(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id"), req.Label, req.Fields)
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	d.credResolver.Invalidate(userIDOf(r.Context()))
	writeJSON(w, http.StatusOK, view)
}

func (d *Daemon) credentialsDelete(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	err := d.creds.Delete(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	d.credResolver.Invalidate(userIDOf(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (d *Daemon) credentialsProviders(w http.ResponseWriter, r *http.Request) {
	cat := credentials.Catalog()
	writeJSON(w, http.StatusOK, map[string]any{"providers": cat, "count": len(cat)})
}

// credentialsModels lists the models the user's stored LLM keys unlock (BYOK),
// fetched live from each provider (cached). Best-effort: a rejected/unsupported
// key just contributes nothing. Drives the BYOK groups in the model picker.
func (d *Daemon) credentialsModels(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	list, offline := d.creds.ListUserModelsWithOffline(r.Context(), userIDOf(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"models": list, "count": len(list), "offline_providers": offline})
}

// credentialsGrants keeps the endpoint alive for the UI. The vault is per-user
// only — there are no grants — so it always reports an empty set.
func (d *Daemon) credentialsGrants(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"grants": []any{}, "count": 0})
}

type credTestRequest struct {
	ProviderName string            `json:"provider_name"`
	ProviderType string            `json:"provider_type"`
	Fields       map[string]string `json:"fields"`
}

// credentialsTest validates the supplied fields against the live provider
// WITHOUT persisting them — drives the "Test" button in the create form. Runs
// on this request's own goroutine with a bounded timeout; never on an agent path.
func (d *Daemon) credentialsTest(w http.ResponseWriter, r *http.Request) {
	var req credTestRequest
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	res := credentials.RunTest(r.Context(), req.ProviderName, req.Fields)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         res.OK,
		"detail":     res.Detail,
		"latency_ms": res.LatencyMS,
	})
}

// credentialsRefresh re-validates a stored credential against the provider and
// records the outcome (status + last_validated_at).
func (d *Daemon) credentialsRefresh(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	res, err := d.creds.Verify(r.Context(), userIDOf(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "refresh_failed", err.Error())
		return
	}
	status := "valid"
	if !res.OK {
		status = "invalid"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         res.OK,
		"detail":     res.Detail,
		"latency_ms": res.LatencyMS,
		"status":     status,
	})
}

// ── GitHub Copilot device flow ───────────────────────────────────

func (d *Daemon) credentialsCopilotStart(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	flow, err := d.creds.CopilotStart(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "copilot_start_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, flow.Public())
}

func (d *Daemon) credentialsCopilotStatus(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "state is required")
		return
	}
	flow, err := d.creds.CopilotPoll(r.Context(), userIDOf(r.Context()), state)
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "unknown or expired device flow")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "copilot_poll_failed", err.Error())
		return
	}
	if flow.CredentialID != "" {
		d.credResolver.Invalidate(userIDOf(r.Context())) // a new github_copilot credential just landed
	}
	writeJSON(w, http.StatusOK, flow.Public())
}

func (d *Daemon) credentialsCopilotModels(w http.ResponseWriter, r *http.Request) {
	if !d.credsReady(w) {
		return
	}
	id, models, err := d.creds.CopilotModels(r.Context(), userIDOf(r.Context()), r.URL.Query().Get("credential_id"))
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found",
			"no github_copilot credential in your vault — run the device flow first")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "copilot_models_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credential_id": id,
		"models":        models,
		"count":         len(models),
	})
}
