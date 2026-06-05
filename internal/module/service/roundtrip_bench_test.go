package service_test

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/module/service"
)

// benchService is a trivial Service that returns a small fixed result, so a
// round-trip measures TRANSPORT + CODEC + gRPC plumbing — not tool work.
type benchService struct{}

func (benchService) Invoke(_ context.Context, req *service.InvokeRequest) (*service.InvokeResponse, error) {
	return &service.InvokeResponse{Result: tool.Result{Success: true, Data: "ok"}, RequestID: req.RequestID}, nil
}

func (benchService) Manifests(context.Context, *service.ManifestsRequest) (*service.ManifestsResponse, error) {
	return &service.ManifestsResponse{}, nil
}

// benchRequest is a representative small tool call (shell.exec with args).
func benchRequest() *service.InvokeRequest {
	params, _ := json.Marshal(map[string]any{
		"command": "ls -la /var/log",
		"timeout": 30,
		"env":     map[string]string{"PATH": "/usr/bin:/bin"},
	})
	return &service.InvokeRequest{
		ModuleID: "shell", ToolName: "exec", Params: params,
		RequestID: "req-0123456789abcdef",
		AppID:     "app-1", SessionID: "sess-1", UserID: "user-1", AgentID: "agent-1",
	}
}

func invokeOnce(ctx context.Context, conn *grpc.ClientConn, req *service.InvokeRequest, out *service.InvokeResponse) error {
	return conn.Invoke(ctx, "/"+service.ServiceName+"/"+service.MethodInvoke, req, out,
		grpc.CallContentSubtype(service.CodecName))
}

func serveOn(b *testing.B, network, address string) (*grpc.ClientConn, func()) {
	b.Helper()
	lis, err := net.Listen(network, address)
	if err != nil {
		b.Skipf("listen %s %q unavailable on this platform: %v", network, address, err)
	}
	srv := grpc.NewServer()
	service.RegisterService(srv, benchService{})
	go func() { _ = srv.Serve(lis) }()

	target := lis.Addr().String()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	conn, err := grpc.NewClient("passthrough:///bench",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		srv.Stop()
		_ = lis.Close()
		b.Fatalf("new client: %v", err)
	}
	return conn, func() { _ = conn.Close(); srv.Stop() }
}

func benchRoundTrip(b *testing.B, conn *grpc.ClientConn) {
	req := benchRequest()
	ctx := context.Background()
	if err := invokeOnce(ctx, conn, req, new(service.InvokeResponse)); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := invokeOnce(ctx, conn, req, new(service.InvokeResponse)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWorker_DirectInvoke is the server-side handler alone — no gRPC,
// no codec, no network. Floor for "the tool itself" (here ~nothing).
func BenchmarkWorker_DirectInvoke(b *testing.B) {
	svc := benchService{}
	req := benchRequest()
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Invoke(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWorker_CodecRoundTrip is the JSON codec cost alone : marshal +
// unmarshal of the request AND the response, no network. The registered
// "json+module" codec is exactly encoding/json (see codec.go), so this
// measures it faithfully.
func BenchmarkWorker_CodecRoundTrip(b *testing.B) {
	req := benchRequest()
	resp := &service.InvokeResponse{Result: tool.Result{Success: true, Data: "ok"}, RequestID: req.RequestID}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rb, err := json.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		var r2 service.InvokeRequest
		if err := json.Unmarshal(rb, &r2); err != nil {
			b.Fatal(err)
		}
		sb, err := json.Marshal(resp)
		if err != nil {
			b.Fatal(err)
		}
		var s2 service.InvokeResponse
		if err := json.Unmarshal(sb, &s2); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWorker_TCP is the full daemon→worker round-trip over TCP
// loopback (transport + codec + gRPC) — the current production path.
func BenchmarkWorker_TCP(b *testing.B) {
	conn, cleanup := serveOn(b, "tcp", "127.0.0.1:0")
	defer cleanup()
	benchRoundTrip(b, conn)
}

// BenchmarkWorker_UDS is the same round-trip over a Unix domain socket
// (AF_UNIX; supported on Windows 10+). Compare ns/op against _TCP to read
// the transport gain directly on this machine.
func BenchmarkWorker_UDS(b *testing.B) {
	sock := filepath.Join(b.TempDir(), "w.sock")
	conn, cleanup := serveOn(b, "unix", sock)
	defer cleanup()
	benchRoundTrip(b, conn)
}
