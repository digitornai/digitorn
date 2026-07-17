package gateway

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/worker"
)

// EnvGatewayAddr is the env var the daemon sets on a worker pool so its
// modules can dial the gateway. Empty/unset → no daemon services
// available to that pool (the historic behaviour for self-contained
// modules like shell/lsp).
const EnvGatewayAddr = "DIGITORN_SERVICE_GATEWAY_ADDR"

// EnvGatewaySecret is the env var carrying the gateway's dedicated
// handshake secret. The per-worker handshake secret is unique per
// instance, so the gateway owns a separate shared secret that the
// daemon hands to every pool authorised to reach it.
const EnvGatewaySecret = "DIGITORN_SERVICE_GATEWAY_SECRET"

// EmbedFunc forwards an embed request to the daemon's embeddings path
// (Manager-routed, health-aware). *embeddings.Client.EmbedRaw fits.
type EmbedFunc func(ctx context.Context, req *embeddings.EmbedRequest) (*embeddings.EmbedResponse, error)

// RerankFunc forwards a rerank request. *embeddings.Client.RerankRaw fits.
type RerankFunc func(ctx context.Context, req *embeddings.RerankRequest) (*embeddings.RerankResponse, error)

// Server is the daemon-side gateway. It owns a loopback gRPC listener
// that worker subprocesses dial.
type Server struct {
	grpc *grpc.Server
	lis  net.Listener
}

// Start binds a loopback listener and serves the gateway in a
// background goroutine. secret is the worker handshake secret callers
// must present ; embed/rerank forward to the daemon's embeddings path.
func Start(secret string, embed EmbedFunc, rerank RerankFunc) (*Server, error) {
	if embed == nil {
		return nil, fmt.Errorf("gateway: nil embed func")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gateway: listen: %w", err)
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(secretUnaryInterceptor(secret)))
	embeddings.RegisterService(s, embedForwarder{embed: embed, rerank: rerank})

	srv := &Server{grpc: s, lis: lis}
	go func() { _ = s.Serve(lis) }()
	return srv, nil
}

// Addr is the loopback address worker pools dial (host:port).
func (s *Server) Addr() string {
	if s == nil || s.lis == nil {
		return ""
	}
	return s.lis.Addr().String()
}

// Stop gracefully shuts the gateway down.
func (s *Server) Stop() {
	if s != nil && s.grpc != nil {
		s.grpc.GracefulStop()
	}
}

// Dial connects to the gateway from a worker subprocess, attaching the
// handshake secret to every call. The returned conn is used to build an
// embeddings.NewDirectClient (and, later, other service clients).
func Dial(addr, secret string) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, fmt.Errorf("gateway: empty address")
	}
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx = metadata.AppendToOutgoingContext(ctx, worker.HeaderSecret, secret)
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
	)
}

// embedForwarder adapts the daemon embeddings path to embeddings.Service.
// Info is a thin pass-through built from a probe embed of the default model.
type embedForwarder struct {
	embed  EmbedFunc
	rerank RerankFunc
}

func (f embedForwarder) Embed(ctx context.Context, req *embeddings.EmbedRequest) (*embeddings.EmbedResponse, error) {
	return f.embed(ctx, req)
}

func (f embedForwarder) Rerank(ctx context.Context, req *embeddings.RerankRequest) (*embeddings.RerankResponse, error) {
	if f.rerank == nil {
		return &embeddings.RerankResponse{}, nil
	}
	return f.rerank(ctx, req)
}

func (f embedForwarder) Info(ctx context.Context, _ *embeddings.InfoRequest) (*embeddings.InfoResponse, error) {
	resp, err := f.embed(ctx, &embeddings.EmbedRequest{})
	if err != nil {
		return nil, err
	}
	return &embeddings.InfoResponse{Model: resp.Model, Dimension: resp.Dimension}, nil
}

// secretUnaryInterceptor mirrors the worker server's auth : the same
// x-digitorn-worker-secret metadata key, validated against the daemon's
// handshake secret.
func secretUnaryInterceptor(expected string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		got := md.Get(worker.HeaderSecret)
		if len(got) == 0 || got[0] != expected {
			return nil, status.Error(codes.Unauthenticated, "bad worker secret")
		}
		return handler(ctx, req)
	}
}
