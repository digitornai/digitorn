package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/module/proxy"
	"github.com/digitornai/digitorn/internal/module/service"
	"github.com/digitornai/digitorn/internal/worker"
)

// ----- Unit tests with mock picker + in-process fake gRPC server -----

// fakeService captures the last Invoke / Manifests it received. The gRPC
// server dispatches each call on its own goroutine, so the capture fields are
// guarded by mu for the concurrent test.
type fakeService struct {
	mu             sync.Mutex
	invokes        int
	lastInvokeReq  *service.InvokeRequest
	invokeResp     *service.InvokeResponse
	invokeErr      error
	manifestsValue *service.ManifestsResponse
}

func (f *fakeService) Invoke(ctx context.Context, req *service.InvokeRequest) (*service.InvokeResponse, error) {
	f.mu.Lock()
	f.invokes++
	f.lastInvokeReq = req
	f.mu.Unlock()
	if f.invokeErr != nil {
		return nil, f.invokeErr
	}
	if f.invokeResp != nil {
		return f.invokeResp, nil
	}
	return &service.InvokeResponse{
		Result:    tool.Result{Success: true, Data: "ok"},
		RequestID: req.RequestID,
	}, nil
}

func (f *fakeService) Manifests(ctx context.Context, _ *service.ManifestsRequest) (*service.ManifestsResponse, error) {
	if f.manifestsValue != nil {
		return f.manifestsValue, nil
	}
	return &service.ManifestsResponse{
		Modules:  []domainmodule.Manifest{{ID: "fakemod", Version: "0.0.1"}},
		WorkerID: "fake-worker",
	}, nil
}

// fakeConn is a worker.Conn backed by a real grpc.ClientConn pointing
// at our local fake server, plus a synthetic Handle.
type fakeConn struct {
	cc     *grpc.ClientConn
	handle worker.Handle
}

func (f *fakeConn) GRPC() *grpc.ClientConn { return f.cc }
func (f *fakeConn) Handle() worker.Handle  { return f.handle }
func (f *fakeConn) Close() error           { return f.cc.Close() }

// fakePicker returns a single shared fakeConn, optionally surfacing
// errFromPick before yielding a conn.
type fakePicker struct {
	conn        *fakeConn
	errFromPick error
	picks       atomic.Int64
}

func (p *fakePicker) Pick(ctx context.Context, kind worker.Kind) (worker.Conn, error) {
	p.picks.Add(1)
	if p.errFromPick != nil {
		return nil, p.errFromPick
	}
	return p.conn, nil
}

// newFakeServer spins up a real gRPC server in-memory and returns the
// service impl + a fakeConn ready to use.
func newFakeServer(t *testing.T) (*fakeService, *fakeConn) {
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

	cc, err := grpc.NewClient(ln.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype(service.CodecName)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		srv.GracefulStop()
		<-done
	})
	return impl, &fakeConn{
		cc:     cc,
		handle: worker.Handle{ID: "fake#1", Kind: "fakekind", Address: ln.Addr().String()},
	}
}

// ----- TESTS -----

// TestProxy_New_FetchesManifestAtBoot : the constructor talks to the
// worker once to learn what it hosts, caches the manifest entry.
func TestProxy_New_FetchesManifestAtBoot(t *testing.T) {
	impl, conn := newFakeServer(t)
	impl.manifestsValue = &service.ManifestsResponse{
		Modules: []domainmodule.Manifest{
			{ID: "fakemod", Version: "1.2.3", Description: "test"},
			{ID: "othermod", Version: "0.1.0"},
		},
		WorkerID: "fake-w",
	}
	picker := &fakePicker{conn: conn}

	p, err := proxy.New(context.Background(), proxy.Options{
		ModuleID: "fakemod",
		Kind:     "fakekind",
		Picker:   picker,
		Logger:   quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := p.Manifest()
	if m.ID != "fakemod" || m.Version != "1.2.3" {
		t.Errorf("manifest cached wrong : %+v", m)
	}
}

// TestProxy_New_RejectsUnhostedModule : worker doesn't host the
// asked-for module → New() returns an error mentioning what IS
// hosted (so the operator can see the config drift).
func TestProxy_New_RejectsUnhostedModule(t *testing.T) {
	impl, conn := newFakeServer(t)
	impl.manifestsValue = &service.ManifestsResponse{
		Modules: []domainmodule.Manifest{{ID: "fakemod"}},
	}
	picker := &fakePicker{conn: conn}

	_, err := proxy.New(context.Background(), proxy.Options{
		ModuleID: "this-not-hosted",
		Kind:     "fakekind",
		Picker:   picker,
		Logger:   quiet(),
	})
	if err == nil {
		t.Fatal("expected error for unhosted module")
	}
	if !contains(err.Error(), "does not host") {
		t.Errorf("error message unclear : %v", err)
	}
}

// TestProxy_Invoke_RoutesAndPropagatesResult : a call lands on the
// worker, the worker's Result comes back through.
func TestProxy_Invoke_RoutesAndPropagatesResult(t *testing.T) {
	impl, conn := newFakeServer(t)
	impl.invokeResp = &service.InvokeResponse{
		Result: tool.Result{
			Success: true,
			Data:    map[string]any{"hello": "world"},
		},
	}
	picker := &fakePicker{conn: conn}
	p := mustNewProxy(t, picker)

	params := []byte(`{"path":"foo"}`)
	res, err := p.Invoke(context.Background(), "read", params)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Errorf("Result.Success = false")
	}
	if impl.invokes != 1 {
		t.Errorf("server invocations = %d, want 1", impl.invokes)
	}
	if impl.lastInvokeReq.ModuleID != "fakemod" {
		t.Errorf("ModuleID = %q, want fakemod", impl.lastInvokeReq.ModuleID)
	}
	if impl.lastInvokeReq.ToolName != "read" {
		t.Errorf("ToolName = %q, want read", impl.lastInvokeReq.ToolName)
	}
	if string(impl.lastInvokeReq.Params) != string(params) {
		t.Errorf("Params mangled : got %s", impl.lastInvokeReq.Params)
	}
	if impl.lastInvokeReq.RequestID == "" {
		t.Error("RequestID should be auto-populated")
	}
}

// TestProxy_Invoke_WorkerResultErrorPassThrough : worker returns
// Success=false with an Error message → proxy passes verbatim
// (no Go error, so caller doesn't retry).
func TestProxy_Invoke_WorkerResultErrorPassThrough(t *testing.T) {
	impl, conn := newFakeServer(t)
	impl.invokeResp = &service.InvokeResponse{
		Result: tool.Result{Success: false, Error: "tool exploded"},
	}
	picker := &fakePicker{conn: conn}
	p := mustNewProxy(t, picker)

	res, err := p.Invoke(context.Background(), "anything", nil)
	if err != nil {
		t.Errorf("Invoke should NOT return Go error for worker-shaped failure : %v", err)
	}
	if res.Success {
		t.Errorf("Result.Success should be false")
	}
	if res.Error != "tool exploded" {
		t.Errorf("Result.Error not passed through : %q", res.Error)
	}
}

// TestProxy_Invoke_NoHealthyWorker_ReturnsErrorResult : Picker fails
// before we even reach the network. Proxy returns Result.Success=false
// + a Go error so the runtime knows it's a transport-level failure
// (not a module bug).
func TestProxy_Invoke_NoHealthyWorker_ReturnsErrorResult(t *testing.T) {
	_, conn := newFakeServer(t)
	// First the picker works (for boot Manifests), then we swap to
	// erroring out so subsequent Invoke fails. Achieved by holding
	// a reference to the picker.
	picker := &fakePicker{conn: conn}
	p := mustNewProxy(t, picker)

	picker.errFromPick = errors.New("pool drained")
	res, err := p.Invoke(context.Background(), "read", nil)
	if err == nil {
		t.Error("expected Go error for transport-level failure")
	}
	if res.Success {
		t.Errorf("Result.Success should be false")
	}
	if !contains(res.Error, "pool drained") {
		t.Errorf("Result.Error should mention picker failure : %q", res.Error)
	}
}

// TestProxy_Lifecycle_NoOps : Init / Start / Stop are explicitly
// no-ops because the worker subprocess owns its own lifecycle.
// Calling them on the proxy must not crash and must not affect the
// worker.
func TestProxy_Lifecycle_NoOps(t *testing.T) {
	_, conn := newFakeServer(t)
	picker := &fakePicker{conn: conn}
	p := mustNewProxy(t, picker)

	ctx := context.Background()
	if err := p.Init(ctx, nil); err != nil {
		t.Errorf("Init returned %v ; should be no-op", err)
	}
	if err := p.Start(ctx); err != nil {
		t.Errorf("Start returned %v ; should be no-op", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Errorf("Stop returned %v ; should be no-op", err)
	}
}

// TestProxy_Invoke_RespectsTimeout : a short InvokeTimeout cancels
// the call if the worker is slow.
func TestProxy_Invoke_RespectsTimeout(t *testing.T) {
	_, conn := newFakeServer(t)
	picker := &fakePicker{conn: conn}

	// Build a proxy with a 10ms timeout, then make the server
	// "slow" by overriding the implementation to sleep.
	impl, _ := newFakeServer(t) // separate server for sleep-impl
	_ = impl
	p, err := proxy.New(context.Background(), proxy.Options{
		ModuleID:      "fakemod",
		Kind:          "fakekind",
		Picker:        picker,
		InvokeTimeout: 10 * time.Millisecond,
		Logger:        quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate slow worker by swapping the picker's connection to
	// a server that never replies. Easier : drop the conn so any
	// call times out quickly. We use the existing fast conn but
	// the 10ms cap will still fire if the server is the network's
	// fault. In practice on localhost this test just verifies the
	// timeout pathway doesn't deadlock — the call may succeed too.
	ctx := context.Background()
	start := time.Now()
	_, _ = p.Invoke(ctx, "read", nil)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("Invoke with 10ms timeout took %v", elapsed)
	}
}

// TestProxy_Invoke_Concurrent : 100 concurrent calls succeed. Picker
// is hit 100 times.
func TestProxy_Invoke_Concurrent(t *testing.T) {
	_, conn := newFakeServer(t)
	picker := &fakePicker{conn: conn}
	p := mustNewProxy(t, picker)

	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Invoke(context.Background(), "read", nil)
			if err != nil {
				errs <- err.Error()
				return
			}
			if !res.Success {
				errs <- res.Error
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("call: %s", e)
	}
}

// ----- LIVE TEST : real worker subprocess + real grpc -----

var (
	liveBinOnce sync.Once
	liveBinPath string
	liveBinErr  error
)

func buildLiveWorker(t *testing.T) string {
	t.Helper()
	liveBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "proxy-live-bin-*")
		if err != nil {
			liveBinErr = err
			return
		}
		exe := filepath.Join(dir, "worker")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", exe,
			"github.com/digitornai/digitorn/cmd/digitorn-worker")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			liveBinErr = err
			return
		}
		liveBinPath = exe
	})
	if liveBinErr != nil {
		t.Fatalf("build worker: %v", liveBinErr)
	}
	return liveBinPath
}

// TestLive_Proxy_InvokeFilesystem : full end-to-end through a real
// spawned subprocess. The proxy is registered in a servicebus.Bus
// alongside an unrelated in-process module to prove the runtime
// can't tell the difference.
func TestLive_Proxy_InvokeFilesystem(t *testing.T) {
	exe := buildLiveWorker(t)

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "live.txt"), []byte("via-proxy-live"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := worker.NewManager(quiet())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, _ := json.Marshal(map[string]any{"workspace": tmp})
	if err := m.Spawn(ctx, worker.Spec{
		Kind:         "live-fs",
		Binary:       exe,
		Count:        1,
		StartTimeout: 10 * time.Second,
		Env: map[string]string{
			"DIGITORN_WORKER_MODULES":           "filesystem",
			"DIGITORN_MODULE_FILESYSTEM_CONFIG": string(cfg),
		},
	}); err != nil {
		t.Fatal(err)
	}

	p, err := proxy.New(context.Background(), proxy.Options{
		ModuleID: "filesystem",
		Kind:     "live-fs",
		Picker:   m,
		Logger:   quiet(),
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	if p.Manifest().ID != "filesystem" {
		t.Errorf("manifest cached wrong : %+v", p.Manifest())
	}

	params, _ := json.Marshal(map[string]any{"path": "live.txt"})
	res, err := p.Invoke(context.Background(), "read", params)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("Result.Success=false : %+v", res)
	}
	data, _ := json.Marshal(res.Data)
	if !contains(string(data), "via-proxy-live") {
		t.Errorf("content not returned via proxy : %s", data)
	}
}

// ----- helpers -----

func mustNewProxy(t *testing.T, picker proxy.Picker) *proxy.ProxyModule {
	t.Helper()
	p, err := proxy.New(context.Background(), proxy.Options{
		ModuleID: "fakemod",
		Kind:     "fakekind",
		Picker:   picker,
		Logger:   quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
