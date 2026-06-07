package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A handler panic must become a clean 500 and never propagate (the test process
// would crash if it did).
func TestPanicRecoverer_HandlerPanicBecomes500(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	h := d.panicRecoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom in handler")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("handler panic must yield 500, got %d", rec.Code)
	}
}

// A non-panicking handler passes through untouched.
func TestPanicRecoverer_PassThrough(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	h := d.panicRecoverer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/y", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("non-panicking handler must pass through, got %d", rec.Code)
	}
}
