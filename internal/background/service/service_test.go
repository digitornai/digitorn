package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/background/store"
)

type noopProc struct{}

func (noopProc) Process(context.Context, store.Job) error { return nil }

func newTestService(t *testing.T) *Service {
	t.Helper()
	cfg := Config{
		DBDriver: "sqlite",
		DBDSN:    filepath.Join(t.TempDir(), "bg.db") + "?_pragma=busy_timeout(5000)",
		HTTPAddr: "127.0.0.1:0",
		Workers:  2,
		LeaseTTL: time.Second,
	}
	build := func(*store.Store) (Setup, error) { return Setup{Processor: noopProc{}}, nil }
	svc, err := New(cfg, build, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = svc.closeDB() })
	return svc
}

func TestService_NewOpensAndMigrates(t *testing.T) {
	svc := newTestService(t)
	// Migration ran → Counts query works on the fresh DB.
	if _, err := svc.store.Counts(context.Background()); err != nil {
		t.Fatalf("store not migrated: %v", err)
	}
}

func TestService_ControlSurface(t *testing.T) {
	svc := newTestService(t)
	h := svc.mux()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz: code=%d body=%s", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/stats", nil))
	body := rr2.Body.String()
	if rr2.Code != http.StatusOK || !strings.Contains(body, `"pool"`) || !strings.Contains(body, `"jobs"`) {
		t.Fatalf("stats: code=%d body=%s", rr2.Code, body)
	}
}
