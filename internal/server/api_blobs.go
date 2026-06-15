package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

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
