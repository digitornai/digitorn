// Package socketio implements ports.RealtimeServer on top of
// github.com/zishang520/socket.io v3 — a faithful Go port of the official
// Socket.IO Node.js server. Compatible with all Socket.IO v4+ clients.
//
// This file wires:
//   - Engine.IO transport (WebSocket + long-polling fallback)
//   - Connection state recovery (clients can resume after brief disconnects)
//   - Namespace routing (one namespace per app for isolation)
//   - Room broadcasting (one room per session/user)
//   - Handshake-time authentication middleware
//   - Panic recovery on every handler
package socketio

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	socket "github.com/zishang520/socket.io/servers/socket/v3"
	siotypes "github.com/zishang520/socket.io/v3/pkg/types"

	"github.com/mbathepaul/digitorn/internal/ports"
)

// Options configures the Socket.IO server.
type Options struct {
	// Path is the endpoint the Socket.IO client connects to (default "/socket.io/").
	Path string
	// Namespace is the Socket.IO namespace where handlers fire (default "/").
	// The legacy Python daemon uses "/events" — match that for client compat.
	Namespace string
	// PingInterval is how often the server pings clients.
	PingInterval time.Duration
	// PingTimeout is how long the server waits for a pong before disconnecting.
	PingTimeout time.Duration
	// ConnectTimeout caps the handshake duration.
	ConnectTimeout time.Duration
	// MaxHTTPBufferSize caps the size of any single HTTP message.
	MaxHTTPBufferSize int64
	// AllowedOrigins is the CORS whitelist for handshake requests. Pass nil to allow same-origin only.
	AllowedOrigins []string
	// RedisURL enables horizontal scaling via the Redis adapter when non-empty.
	// (Plumbing left as TODO — wire socket.io-go-redis when needed.)
	RedisURL string
	// MaxDisconnectionDuration enables Connection State Recovery — clients can
	// briefly drop and resume without losing buffered packets. Zero disables it.
	MaxDisconnectionDuration time.Duration
}

// DefaultOptions returns a production-leaning configuration.
func DefaultOptions() Options {
	return Options{
		Path:                     "/socket.io/",
		PingInterval:             25 * time.Second,
		PingTimeout:              20 * time.Second,
		ConnectTimeout:           45 * time.Second,
		MaxHTTPBufferSize:        1_000_000,
		MaxDisconnectionDuration: 2 * time.Minute,
	}
}

// Server wraps zishang520's *socket.Server and implements ports.RealtimeServer.
//
// Every callback (auth, connection, event) runs inside a recover() so a buggy
// or malicious client cannot panic the daemon.
type Server struct {
	io        *socket.Server
	logger    *slog.Logger
	defaultNS string

	mu             sync.RWMutex
	authHandler    ports.AuthHandler
	connHandlers   []ports.ConnectionHandler
	disconnHandlers []ports.DisconnectHandler
	eventHandlers  map[string][]ports.EventHandler
}

// New constructs the Socket.IO server with the given options. Use Handler() to
// mount it on the HTTP router.
func New(opts Options, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Path == "" {
		opts.Path = "/socket.io/"
	}

	so := socket.DefaultServerOptions()
	so.SetPath(opts.Path)
	if opts.ConnectTimeout > 0 {
		so.SetConnectTimeout(opts.ConnectTimeout)
	}
	if opts.PingInterval > 0 {
		so.SetPingInterval(opts.PingInterval)
	}
	if opts.PingTimeout > 0 {
		so.SetPingTimeout(opts.PingTimeout)
	}
	if opts.MaxHTTPBufferSize > 0 {
		so.SetMaxHttpBufferSize(opts.MaxHTTPBufferSize)
	}
	if len(opts.AllowedOrigins) > 0 {
		so.SetCors(corsFromOrigins(opts.AllowedOrigins))
	}
	if opts.MaxDisconnectionDuration > 0 {
		recovery := socket.DefaultConnectionStateRecovery()
		recovery.SetMaxDisconnectionDuration(int64(opts.MaxDisconnectionDuration / time.Millisecond))
		so.SetConnectionStateRecovery(recovery)
	}

	io := socket.NewServer(nil, so)

	ns := opts.Namespace
	if ns == "" {
		ns = "/"
	}

	s := &Server{
		io:            io,
		logger:        logger,
		defaultNS:     ns,
		eventHandlers: make(map[string][]ports.EventHandler),
	}
	s.installRootHandlers()
	return s
}

// Handler returns the http.Handler to mount on the HTTP router (e.g., Chi).
// Mount it at the same path configured in Options.Path:
//
//	router.Handle("/socket.io/*", srv.Handler())
func (s *Server) Handler() http.Handler {
	return s.io.ServeHandler(nil)
}

// IO exposes the underlying socket.Server for advanced operations not covered
// by the ports.RealtimeServer interface (e.g., custom adapters, FetchSockets).
// Use sparingly to keep the rest of the codebase library-agnostic.
func (s *Server) IO() *socket.Server { return s.io }

// SetAuthHandler installs a connection-time auth check. Returning an error
// from the handler rejects the connection. Called once at startup.
func (s *Server) SetAuthHandler(h ports.AuthHandler) {
	s.mu.Lock()
	s.authHandler = h
	s.mu.Unlock()

	// Install as a Socket.IO middleware on the configured namespace.
	s.namespace(s.defaultNS).Use(func(client *socket.Socket, next func(*socket.ExtendedError)) {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("socketio: auth handler panic",
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
				next(socket.NewExtendedError("internal error", nil))
			}
		}()

		s.mu.RLock()
		ah := s.authHandler
		s.mu.RUnlock()
		if ah == nil {
			next(nil)
			return
		}

		hs := client.Handshake()
		var token string
		var metadata map[string]any
		if hs != nil && hs.Auth != nil {
			metadata = hs.Auth
			if t, ok := hs.Auth["token"].(string); ok {
				token = t
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ah(ctx, token, metadata); err != nil {
			s.logger.Info("socketio: auth rejected",
				slog.String("client_id", string(client.Id())),
				slog.String("err", err.Error()),
			)
			next(socket.NewExtendedError(err.Error(), nil))
			return
		}
		next(nil)
	})
}

// OnConnection registers a handler invoked for every new client connection.
// Multiple handlers may be registered; each runs in its own recovered goroutine.
func (s *Server) OnConnection(h ports.ConnectionHandler) {
	s.mu.Lock()
	s.connHandlers = append(s.connHandlers, h)
	s.mu.Unlock()
}

// OnDisconnection registers a handler invoked when a client disconnects.
func (s *Server) OnDisconnection(h ports.DisconnectHandler) {
	s.mu.Lock()
	s.disconnHandlers = append(s.disconnHandlers, h)
	s.mu.Unlock()
}

// OnEvent registers a handler for a named event. The handler fires for any
// client emitting the event on the default namespace.
func (s *Server) OnEvent(event string, h ports.EventHandler) {
	s.mu.Lock()
	s.eventHandlers[event] = append(s.eventHandlers[event], h)
	s.mu.Unlock()
}

// Emit sends an event to a specific room. If namespace is "", the configured
// default namespace is used. If room is "", the event is broadcast to the namespace.
func (s *Server) Emit(ctx context.Context, namespace, room, event string, data any) error {
	if namespace == "" {
		namespace = s.defaultNS
	}
	ns := s.namespace(namespace)
	if room != "" {
		return ns.To(socket.Room(room)).Emit(event, data)
	}
	return ns.Emit(event, data)
}

// Broadcast sends an event to every client in the namespace.
func (s *Server) Broadcast(ctx context.Context, namespace, event string, data any) error {
	if namespace == "" {
		namespace = s.defaultNS
	}
	return s.namespace(namespace).Emit(event, data)
}

// Close gracefully disconnects all clients and stops the server.
func (s *Server) Close(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		// Disconnect everyone, closing the underlying transports.
		s.io.DisconnectSockets(true)
		// The library does not expose a synchronous Close; the engine.io
		// transport closes via DisconnectSockets above. We simply yield.
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("socketio: close timeout: %w", ctx.Err())
	}
}

// --- Internal wiring ---

func (s *Server) namespace(name string) socket.Namespace {
	if name == "" || name == "/" {
		return s.io.Sockets()
	}
	return s.io.Of(name, nil)
}

// installRootHandlers wires the library "connection" event on the configured
// namespace to our typed handlers, with recover() on every callback.
func (s *Server) installRootHandlers() {
	s.namespace(s.defaultNS).On("connection", func(clients ...any) {
		if len(clients) == 0 {
			return
		}
		raw, ok := clients[0].(*socket.Socket)
		if !ok {
			s.logger.Error("socketio: unexpected connection payload type")
			return
		}
		c := newClient(raw, s.logger)

		// Pre-wire registered named-event handlers.
		s.mu.RLock()
		eventHandlers := make(map[string][]ports.EventHandler, len(s.eventHandlers))
		for k, v := range s.eventHandlers {
			eventHandlers[k] = append([]ports.EventHandler(nil), v...)
		}
		connHandlers := append([]ports.ConnectionHandler(nil), s.connHandlers...)
		s.mu.RUnlock()

		for event, handlers := range eventHandlers {
			event, handlers := event, handlers
			raw.On(event, func(args ...any) {
				var data any
				if len(args) == 1 {
					data = args[0]
				} else if len(args) > 1 {
					data = args
				}
				// CARDINAL RULE : the event loop must NEVER block. Run every handler
				// OFF the socket reader callback in its own goroutine so a slow or
				// blocking handler — a cold session load (hydrate + full replay), a
				// JSONL read, a disk flush, an LLM call — can never stall the loop,
				// freeze other clients, or wedge this client's pings. Each inbound
				// event is self-contained (data-driven, no cross-event ordering needed
				// for delivery), per-client state is mutex-guarded, and the bus has its
				// own concurrency control, so independent processing is safe. Bounded
				// by the 30s per-event timeout below.
				go func() {
					defer func() {
						if r := recover(); r != nil {
							s.logger.Error("socketio: event handler panic",
								slog.String("event", event),
								slog.String("client_id", string(raw.Id())),
								slog.Any("panic", r),
								slog.String("stack", string(debug.Stack())),
							)
						}
					}()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					for _, h := range handlers {
						if err := h(ctx, c, data); err != nil {
							s.logger.Warn("socketio: event handler error",
								slog.String("event", event),
								slog.String("err", err.Error()),
							)
						}
					}
				}()
			})
		}

		// Disconnect : log + invoke registered disconnect handlers so the
		// bridge can free per-connection state (otherwise clientState leaks
		// for the lifetime of the process).
		raw.On("disconnect", func(args ...any) {
			reason := "unknown"
			if len(args) > 0 {
				if r, ok := args[0].(string); ok {
					reason = r
				}
			}
			s.logger.Info("socketio: client disconnected",
				slog.String("client_id", string(raw.Id())),
				slog.String("reason", reason),
			)
			s.mu.RLock()
			handlers := append([]ports.DisconnectHandler(nil), s.disconnHandlers...)
			s.mu.RUnlock()
			id := string(raw.Id())
			for _, h := range handlers {
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.logger.Error("socketio: disconnect handler panic",
								slog.String("client_id", id), slog.Any("panic", r))
						}
					}()
					h(id)
				}()
			}
		})

		// Invoke user connection handlers
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("socketio: connection handler panic",
						slog.String("client_id", string(raw.Id())),
						slog.Any("panic", r),
						slog.String("stack", string(debug.Stack())),
					)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, h := range connHandlers {
				if err := h(ctx, c); err != nil {
					s.logger.Warn("socketio: connection handler error",
						slog.String("client_id", string(raw.Id())),
						slog.String("err", err.Error()),
					)
				}
			}
		}()

		s.logger.Info("socketio: client connected",
			slog.String("client_id", string(raw.Id())),
		)
	})
}

// corsFromOrigins builds the *types.Cors that socket.io expects.
func corsFromOrigins(origins []string) *siotypes.Cors {
	// Wildcard handling has TWO traps with this socket.io lib:
	//  1. IsOriginAllowed treats a STRING "*" as allow-all, but a []string{"*"}
	//     is matched LITERALLY — so a real Origin never matches and the
	//     handshake is rejected with HTTP 403. Pass the bare "*" string.
	//  2. `Access-Control-Allow-Origin: *` + `Allow-Credentials: true` is
	//     rejected by browsers, so credentials must be off for the wildcard.
	// The socket carries its token in the handshake `auth` payload (not a
	// cookie), so credentials aren't needed anyway.
	for _, o := range origins {
		if o == "*" {
			return &siotypes.Cors{Origin: "*", Credentials: false}
		}
	}
	return &siotypes.Cors{Origin: origins, Credentials: true}
}

var _ ports.RealtimeServer = (*Server)(nil)
