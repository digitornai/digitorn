// Package http builds the Chi-based REST HTTP server.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// Options configures the HTTP server.
type Options struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	CORSOrigins     []string
}

// Server wraps net/http with a Chi router and idiomatic middleware.
type Server struct {
	srv    *nethttp.Server
	router *chi.Mux
	opts   Options
	logger *slog.Logger
}

// New creates a Server with default middleware: RequestID, Recoverer, Logger
// (via slog), Timeout, and CORS.
func New(opts Options, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 30 * time.Second
	}
	// WriteTimeout intentionally defaults to 0 (disabled): this listener is
	// shared with Socket.IO and SSE, whose responses are long-lived. A blanket
	// write deadline would sever those streams. Per-request bounds for the REST
	// routes come from the chi Timeout middleware below.
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 30 * time.Second
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(slogRequestLogger(logger))
	r.Use(middleware.Timeout(60 * time.Second))
	// A wildcard origin combined with credentialed requests is forbidden by the
	// browser CORS spec and would silently disable credentials anyway. Only
	// allow credentials when the origin set is explicit.
	allowCreds := len(opts.CORSOrigins) > 0
	for _, o := range opts.CORSOrigins {
		if o == "*" {
			allowCreds = false
			break
		}
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   opts.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		// Echo whatever headers the (allow-listed) origin requests. The origin
		// allow-list is the security boundary; restricting headers on top only
		// breaks legitimate front-ends that send custom headers (X-User-ID, …).
		AllowedHeaders:   []string{"*"},
		ExposedHeaders:   []string{"Link", "X-Request-ID"},
		AllowCredentials: allowCreds,
		MaxAge:           300,
	}))

	srv := &nethttp.Server{
		Addr:              opts.Addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       opts.ReadTimeout,
		WriteTimeout:      opts.WriteTimeout,
		IdleTimeout:       120 * time.Second,
	}

	s := &Server{srv: srv, router: r, opts: opts, logger: logger}
	s.registerSystemRoutes()
	return s
}

// Router exposes the Chi router for mounting application routes.
func (s *Server) Router() chi.Router { return s.router }

// HTTPServer returns the underlying *http.Server (useful to share the listener
// with Socket.IO, which exposes its own handler).
func (s *Server) HTTPServer() *nethttp.Server { return s.srv }

// Start blocks until the server stops.
func (s *Server) Start() error {
	s.logger.Info("http: listening", slog.String("addr", s.opts.Addr))
	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
		return fmt.Errorf("http: serve: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http: shutdown: %w", err)
	}
	return nil
}

// registerSystemRoutes adds /healthz, /readyz, /version. Application routes
// must be mounted by the caller via Router().
func (s *Server) registerSystemRoutes() {
	s.router.Get("/healthz", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
	s.router.Get("/readyz", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ready"})
	})
}

func writeJSON(w nethttp.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// slogRequestLogger logs each request with structured fields. Replaces Chi's
// stdlib-based middleware.Logger.
func slogRequestLogger(logger *slog.Logger) func(nethttp.Handler) nethttp.Handler {
	return func(next nethttp.Handler) nethttp.Handler {
		return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.Info("http",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
