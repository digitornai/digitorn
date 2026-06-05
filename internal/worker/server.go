package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ServerConfig is what a worker binary passes to Run() to start its gRPC
// server. The framework handles: binding a port, the handshake header
// validation, the gRPC Health service, the ready-line on stdout, and
// graceful SIGTERM handling.
type ServerConfig struct {
	// Handshake is what ReadEnvHandshake() returned. The Secret value
	// is used to validate every incoming RPC.
	Handshake HandshakeContext

	// BindAddr is the TCP address to bind. Use "127.0.0.1:0" to let
	// the OS pick a port — the chosen port is published on stdout.
	BindAddr string

	// Register is called after server construction but before Serve.
	// It is where the worker binary registers its own gRPC services
	// (e.g., the LLM service in L3).
	Register func(*grpc.Server)

	// HealthSetter lets the worker flip its Health=SERVING once ready.
	// If nil, the framework marks the default service SERVING right
	// after Serve() returns from BindAddr.
	HealthSetter func(*health.Server)
}

// Run starts the gRPC server with handshake validation, health service,
// and graceful shutdown on SIGTERM/SIGINT. It blocks until the server
// stops or the OS signal arrives. Errors propagate up to the caller (the
// worker binary's main()).
func Run(cfg ServerConfig) error {
	if cfg.Handshake.Secret == "" {
		return ErrAuthBadSecret
	}
	addr := cfg.BindAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	// A "unix:<path>" address binds an AF_UNIX socket (faster same-host
	// IPC) ; anything else is a TCP address. A trailing separator means
	// "<path> is a directory — pick a unique socket inside it", so a pool
	// of N instances sharing one env doesn't collide on a single path.
	network, listenAddr := "tcp", addr
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, listenAddr = "unix", p
		if strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(os.PathSeparator)) {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return fmt.Errorf("worker: mkdir socket dir %s: %w", p, err)
			}
			listenAddr = filepath.Join(p, fmt.Sprintf("w-%d.sock", os.Getpid()))
		}
	}
	if network == "unix" {
		// Clear a stale socket left by a crashed predecessor : net.Listen("unix")
		// fails with EADDRINUSE if the file already exists, so a hard-killed worker
		// whose PID is later reused would never rebind. The name is PID-unique, so
		// any file here belongs to a dead process — safe to remove. A clean
		// shutdown unlinks the socket itself (Go's UnixListener.Close), and the
		// defer below covers the paths where Close doesn't run.
		_ = os.Remove(listenAddr)
		defer func() { _ = os.Remove(listenAddr) }()
	}
	ln, err := net.Listen(network, listenAddr)
	if err != nil {
		return fmt.Errorf("worker: listen %s %s: %w", network, listenAddr, err)
	}

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(secretUnaryInterceptor(cfg.Handshake.Secret)),
		grpc.StreamInterceptor(secretStreamInterceptor(cfg.Handshake.Secret)),
	)

	hsrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hsrv)
	hsrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	if cfg.Register != nil {
		cfg.Register(srv)
	}

	// Publish the bound address on stdout so the master can discover it.
	// Unix sockets are scheme-qualified so the master dials AF_UNIX too.
	published := ln.Addr().String()
	if network == "unix" {
		published = "unix:" + published
	}
	fmt.Println(readyLinePrefix + published)

	if cfg.HealthSetter != nil {
		cfg.HealthSetter(hsrv)
	} else {
		hsrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	}

	// Goroutine : OS-signal → GracefulStop with a hard-kill safety net.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stopDone := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		<-sigCh
		stopOnce.Do(func() {
			hsrv.Shutdown()
			srv.GracefulStop()
			close(stopDone)
		})
	}()

	serveErr := srv.Serve(ln)
	stopOnce.Do(func() {
		hsrv.Shutdown()
		close(stopDone)
	})
	<-stopDone
	if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return serveErr
	}
	return nil
}

func secretUnaryInterceptor(expected string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := validateSecretCtx(ctx, expected); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func secretStreamInterceptor(expected string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := validateSecretCtx(ss.Context(), expected); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func validateSecretCtx(ctx context.Context, expected string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	got := md.Get(HeaderSecret)
	if len(got) == 0 || got[0] != expected {
		return status.Error(codes.Unauthenticated, "bad worker secret")
	}
	return nil
}
