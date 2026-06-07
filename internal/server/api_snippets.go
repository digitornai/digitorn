package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/usersnippets"
)

// listSnippets returns the caller's saved prompts for an app, newest first.
func (d *Daemon) listSnippets(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	items, err := d.userSnippets.List(r.Context(), userID, appID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snippets": items,
		"count":    len(items),
	})
}

// createSnippet saves a new prompt for the caller + this app.
func (d *Daemon) createSnippet(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	var req struct {
		Title string   `json:"title"`
		Body  string   `json:"body"`
		Emoji string   `json:"emoji"`
		Tags  []string `json:"tags"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sn, err := d.userSnippets.Create(r.Context(), userID, appID, req.Title, req.Body, req.Emoji, req.Tags)
	if err != nil {
		writeSnippetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"snippet": sn})
}

// updateSnippet applies a partial change to one of the caller's snippets.
func (d *Daemon) updateSnippet(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	snippetID := chi.URLParam(r, "snippet_id")
	userID := userIDOf(r.Context())
	var req struct {
		Title *string   `json:"title"`
		Body  *string   `json:"body"`
		Emoji *string   `json:"emoji"`
		Tags  *[]string `json:"tags"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sn, err := d.userSnippets.Update(r.Context(), userID, appID, snippetID, req.Title, req.Body, req.Emoji, req.Tags)
	if err != nil {
		writeSnippetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snippet": sn})
}

// deleteSnippet hard-deletes one of the caller's snippets.
func (d *Daemon) deleteSnippet(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	snippetID := chi.URLParam(r, "snippet_id")
	userID := userIDOf(r.Context())
	if err := d.userSnippets.Delete(r.Context(), userID, appID, snippetID); err != nil {
		writeSnippetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// writeSnippetError maps the store's sentinel errors to HTTP status codes.
func writeSnippetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, usersnippets.ErrNotFound):
		writeError(w, http.StatusNotFound, "snippet_not_found", err.Error())
	case errors.Is(err, usersnippets.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_snippet", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "snippet_failed", err.Error())
	}
}
