package server

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// MountAuthProxy registers `/auth/*` to redirect (HTTP 308) to the external
// auth service. This mirrors the Python daemon's behaviour exactly :
// the daemon never serves login/logout/refresh itself, it just points
// clients at the configured service. Query string preserved.
func MountAuthProxy(r chi.Router, serviceURL string) {
	if serviceURL == "" {
		return
	}
	base := strings.TrimRight(serviceURL, "/")
	handler := func(w http.ResponseWriter, req *http.Request) {
		rest := chi.URLParam(req, "*")
		target := base + "/auth/" + rest
		if qs := req.URL.RawQuery; qs != "" {
			target += "?" + qs
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusPermanentRedirect)
	}
	for _, method := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		r.Method(method, "/auth/*", http.HandlerFunc(handler))
	}
}
