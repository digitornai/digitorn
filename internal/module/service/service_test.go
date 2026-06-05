package service_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/module/service"
)

// fakeService is a deterministic Service used to test the gRPC wire :
// it echoes ModuleID + ToolName into the result so the test can assert
// the request reached the server intact.
type fakeService struct {
	invokes    int
	manifests  int
	lastReq    *service.InvokeRequest
	manifestsV []domainmodule.Manifest
}

func (f *fakeService) Invoke(ctx context.Context, req *service.InvokeRequest) (*service.InvokeResponse, error) {
	f.invokes++
	f.lastReq = req
	return &service.InvokeResponse{
		Result: tool.Result{
			Success: true,
			Data: map[string]any{
				"echo_module": req.ModuleID,
				"echo_tool":   req.ToolName,
				"params_len":  len(req.Params),
			},
		},
		RequestID:  req.RequestID,
		DurationMs: 1,
	}, nil
}

func (f *fakeService) Manifests(ctx context.Context, req *service.ManifestsRequest) (*service.ManifestsResponse, error) {
	f.manifests++
	return &service.ManifestsResponse{Modules: f.manifestsV, WorkerID: "test-worker"}, nil
}

// newClient builds a grpc dialer for the in-memory test server.
func newClient(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype(service.CodecName)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newServer wires a fake Service onto a real grpc.Server on a random
// loopback port. Returns the server + addr + impl for assertions.
func newServer(t *testing.T) (*grpc.Server, string, *fakeService) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	impl := &fakeService{}
	service.RegisterService(srv, impl)

	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() {
		srv.GracefulStop()
		<-done
	})
	return srv, ln.Addr().String(), impl
}

// TestService_CodecRegistered checks the package's init() ran and the
// "json+module" codec is selectable from gRPC.
func TestService_CodecRegistered(t *testing.T) {
	if c := encoding.GetCodec(service.CodecName); c == nil {
		t.Fatalf("codec %q not registered", service.CodecName)
	}
}

// TestService_ServiceDesc_Methods checks the manual ServiceDesc has the
// expected method names and the service-name on the wire.
func TestService_ServiceDesc_Methods(t *testing.T) {
	srv := grpc.NewServer()
	impl := &fakeService{}
	service.RegisterService(srv, impl)

	info := srv.GetServiceInfo()
	desc, ok := info[service.ServiceName]
	if !ok {
		t.Fatalf("service %q not registered ; got %v", service.ServiceName, keys(info))
	}
	names := map[string]bool{}
	for _, m := range desc.Methods {
		names[m.Name] = true
	}
	for _, want := range []string{service.MethodInvoke, service.MethodManifests} {
		if !names[want] {
			t.Errorf("missing method %s", want)
		}
	}
}

// TestService_Invoke_EndToEnd runs a real gRPC round-trip and asserts
// the request reached the handler verbatim and the response made it
// back through the JSON codec.
func TestService_Invoke_EndToEnd(t *testing.T) {
	_, addr, impl := newServer(t)
	conn := newClient(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	params, _ := json.Marshal(map[string]any{"path": "foo.txt"})
	req := &service.InvokeRequest{
		ModuleID:   "filesystem",
		ToolName:   "read",
		Params:     params,
		RequestID:  "req-1",
		DeadlineMs: 1000,
	}
	out := new(service.InvokeResponse)
	if err := conn.Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodInvoke,
		req, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		t.Fatalf("Invoke RPC: %v", err)
	}

	if impl.invokes != 1 {
		t.Errorf("server invocations = %d, want 1", impl.invokes)
	}
	if impl.lastReq.ModuleID != "filesystem" || impl.lastReq.ToolName != "read" {
		t.Errorf("request mangled : %+v", impl.lastReq)
	}
	if impl.lastReq.RequestID != "req-1" {
		t.Errorf("RequestID lost on the wire : %q", impl.lastReq.RequestID)
	}
	if string(impl.lastReq.Params) != string(params) {
		t.Errorf("Params mangled : got %s want %s", impl.lastReq.Params, params)
	}
	if !out.Result.Success {
		t.Errorf("Result.Success = false ; data = %v", out.Result.Data)
	}
	if out.RequestID != "req-1" {
		t.Errorf("RequestID not echoed back : %q", out.RequestID)
	}
}

// TestService_Manifests_EndToEnd : the worker advertises its modules ;
// the daemon decodes the list.
func TestService_Manifests_EndToEnd(t *testing.T) {
	_, addr, impl := newServer(t)
	impl.manifestsV = []domainmodule.Manifest{
		{ID: "shell", Version: "1.0.0"},
		{ID: "filesystem", Version: "1.0.0"},
	}
	conn := newClient(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := new(service.ManifestsResponse)
	if err := conn.Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodManifests,
		&service.ManifestsRequest{}, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		t.Fatalf("Manifests RPC: %v", err)
	}
	if impl.manifests != 1 {
		t.Errorf("server manifests calls = %d, want 1", impl.manifests)
	}
	if len(out.Modules) != 2 {
		t.Fatalf("got %d modules, want 2", len(out.Modules))
	}
	if out.WorkerID != "test-worker" {
		t.Errorf("WorkerID lost on the wire : %q", out.WorkerID)
	}
}

// TestService_JSONCodec_RoundTrip exercises only the codec path so
// breakage there is caught even without a server.
func TestService_JSONCodec_RoundTrip(t *testing.T) {
	c := encoding.GetCodec(service.CodecName)
	if c == nil {
		t.Fatal("codec missing")
	}
	in := &service.InvokeRequest{
		ModuleID: "lsp",
		ToolName: "diagnose",
		Params:   json.RawMessage(`{"file":"index.ts"}`),
	}
	buf, err := c.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &service.InvokeRequest{}
	if err := c.Unmarshal(buf, out); err != nil {
		t.Fatal(err)
	}
	if out.ModuleID != in.ModuleID || out.ToolName != in.ToolName ||
		string(out.Params) != string(in.Params) {
		t.Errorf("round-trip mismatch :\nin :%+v\nout:%+v", in, out)
	}
}

// TestService_EmptyParamsAcceptedAsNull guarantees the wire stays
// stable when Params is omitted — important because most tools don't
// have any input (e.g. shell.ps, memory.list).
func TestService_EmptyParamsAcceptedAsNull(t *testing.T) {
	_, addr, impl := newServer(t)
	conn := newClient(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &service.InvokeRequest{ModuleID: "shell", ToolName: "ps"}
	out := new(service.InvokeResponse)
	if err := conn.Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodInvoke,
		req, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(impl.lastReq.Params) != 0 {
		t.Errorf("empty Params should arrive as nil/0-length, got %d bytes", len(impl.lastReq.Params))
	}
	if !out.Result.Success {
		t.Errorf("Result.Success = false")
	}
}

// helper : extract keys of a service-info map.
func keys(m map[string]grpc.ServiceInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
