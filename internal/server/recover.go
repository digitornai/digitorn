package server

import (
	"fmt"
	"net/http"
	"runtime/debug"
)

// panicRecoverer turns any panic in a downstream HTTP handler into a clean 500
// + a structured log, so a single bad request can never abort the connection
// ungracefully or risk the daemon process. Cardinal rule: jamais crash.
func (d *Daemon) panicRecoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the sanctioned "abort this request" signal
			// (used by the reverse-proxy / flusher) — re-panic so net/http handles
			// it as intended instead of logging a fake error.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			if d.logger != nil {
				d.logger.Error("panic in HTTP handler",
					"method", r.Method, "path", r.URL.Path,
					"panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
