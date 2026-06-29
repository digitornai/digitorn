package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// isHexHash reports whether s is a 64-char lowercase-hex SHA-256 — the only
// shape the content-addressed store emits. Rejecting anything else keeps path
// traversal out of the blob lookup.
func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// uploadBlob ingests raw bytes into the content-addressed blob store and returns a
// BlobRef {hash, mime, size}. It is the GENERIC inbound-media primitive: any client
// (web, CLI, background channels, voice) uploads bytes here, then references the
// returned hash in a message's attachments — the daemon's multipart adapter resolves
// each BlobRef into vision/audio content the model actually sees. App-scoped (the
// route group is authenticated) so a blob can exist BEFORE its session is created;
// the security boundary is the owned session the BlobRef is later attached to. Body
// streamed to disk and bounded.
func (d *Daemon) uploadBlob(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if d.appMgr != nil {
		if app, err := d.appMgr.Get(r.Context(), appID); err != nil || app == nil {
			writeError(w, http.StatusNotFound, "app_not_found", "app not installed or not enabled")
			return
		}
	}
	if d.blobStore == nil {
		writeError(w, http.StatusServiceUnavailable, "blobstore_unavailable", "blob store not wired")
		return
	}
	mime := r.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	body := http.MaxBytesReader(w, r.Body, 32<<20) // 32 MB ceiling, streamed
	ref, err := d.blobStore.Put(r.Context(), mime, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "blob_put_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

// getBlob streams a stored blob by its content-address hash. Same app-scoped
// auth as uploadBlob (the route group is authenticated). The ``mime`` query
// param sets Content-Type — the caller already holds it from the message part;
// the store doesn't need to persist it separately. Content-addressed bytes are
// immutable, so the response is cacheable forever.
func (d *Daemon) getBlob(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	if d.appMgr != nil {
		if app, err := d.appMgr.Get(r.Context(), appID); err != nil || app == nil {
			writeError(w, http.StatusNotFound, "app_not_found", "app not installed or not enabled")
			return
		}
	}
	if d.blobStore == nil {
		writeError(w, http.StatusServiceUnavailable, "blobstore_unavailable", "blob store not wired")
		return
	}
	hash := chi.URLParam(r, "hash")
	if !isHexHash(hash) {
		writeError(w, http.StatusBadRequest, "bad_hash", "invalid blob hash")
		return
	}
	rc, err := d.blobStore.Get(r.Context(), hash)
	if err != nil {
		writeError(w, http.StatusNotFound, "blob_not_found", err.Error())
		return
	}
	defer rc.Close()
	// Sanitise the caller-supplied mime (header-injection guard).
	mime := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, r.URL.Query().Get("mime"))
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
