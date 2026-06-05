package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/module/service"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// ----- Helpers : compile the binary once, share across tests -----

var (
	workerBinOnce sync.Once
	workerBinPath string
	workerBinErr  error
)

// buildWorkerBinary compiles cmd/digitorn-worker once for the whole
// package run and returns the path. Slow on first call (≈3s), free
// after.
func buildWorkerBinary(t *testing.T) string {
	t.Helper()
	workerBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "digitorn-worker-bin-*")
		if err != nil {
			workerBinErr = err
			return
		}
		exe := filepath.Join(dir, "worker")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", exe,
			"github.com/mbathepaul/digitorn/cmd/digitorn-worker")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			workerBinErr = err
			return
		}
		workerBinPath = exe
	})
	if workerBinErr != nil {
		t.Fatalf("build digitorn-worker: %v", workerBinErr)
	}
	return workerBinPath
}

// quietLogger discards logs so tests don't litter stdout.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// spawnPool spawns a worker pool of the given kind serving the given
// modules. Returns the worker.Manager + a function that picks one
// of the workers and returns a connected client.
func spawnPool(t *testing.T, kind worker.Kind, modules string, count int) (*worker.Manager, func() worker.Conn) {
	t.Helper()
	exe := buildWorkerBinary(t)
	m := worker.NewManager(quietLogger())
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
	if err := m.Spawn(ctx, worker.Spec{
		Kind:         kind,
		Binary:       exe,
		Count:        count,
		StartTimeout: 10 * time.Second,
		Env: map[string]string{
			"DIGITORN_WORKER_MODULES": modules,
		},
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	pick := func() worker.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := m.Pick(ctx, kind)
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		return c
	}
	return m, pick
}

// invoke fires one Invoke RPC against a worker conn.
func invoke(t *testing.T, c worker.Conn, req *service.InvokeRequest) *service.InvokeResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := new(service.InvokeResponse)
	if err := c.GRPC().Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodInvoke,
		req, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		t.Fatalf("Invoke RPC: %v", err)
	}
	return out
}

// manifests fires one Manifests RPC against a worker conn.
func manifests(t *testing.T, c worker.Conn) *service.ManifestsResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := new(service.ManifestsResponse)
	if err := c.GRPC().Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodManifests,
		&service.ManifestsRequest{}, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		t.Fatalf("Manifests RPC: %v", err)
	}
	return out
}

// ----- TESTS -----

// TestWorker_HostsFilesystemAndShell : the worker boots, serves both
// modules, advertises them via Manifests.
func TestWorker_HostsFilesystemAndShell(t *testing.T) {
	_, pick := spawnPool(t, "pool-fs-shell", "filesystem,shell", 1)

	resp := manifests(t, pick())
	if len(resp.Modules) != 2 {
		t.Fatalf("got %d manifests, want 2 ; ids = %v", len(resp.Modules), manifestIDs(resp.Modules))
	}
	ids := manifestIDs(resp.Modules)
	if !contains(ids, "filesystem") || !contains(ids, "shell") {
		t.Errorf("manifests missing expected ids : got %v", ids)
	}
	if resp.WorkerID == "" {
		t.Error("WorkerID empty in Manifests response")
	}
}

// TestWorker_EmptyModulesListMeansNothing : DIGITORN_WORKER_MODULES
// unset → worker is alive but Manifests is empty. Useful smoke for
// "did the worker start at all".
func TestWorker_EmptyModulesListMeansNothing(t *testing.T) {
	_, pick := spawnPool(t, "pool-empty", "", 1)

	resp := manifests(t, pick())
	if len(resp.Modules) != 0 {
		t.Errorf("expected 0 modules, got %d", len(resp.Modules))
	}
}

// TestWorker_InvokeFilesystemRead_RoundTrip : full path from daemon
// (this test) → worker subprocess → bus → filesystem module → file
// content back. The single most important contract proof for D3.
func TestWorker_InvokeFilesystemRead_RoundTrip(t *testing.T) {
	// Spawn a worker hosting filesystem, pointed at a workspace dir
	// pre-populated with a file we'll ask it to read.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "hello.txt"), []byte("digitorn-worker-live"), 0o644); err != nil {
		t.Fatal(err)
	}

	exe := buildWorkerBinary(t)
	m := worker.NewManager(quietLogger())
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := `{"workspace":"` + jsonEscapePath(tmp) + `","max_file_bytes":1048576}`
	if err := m.Spawn(ctx, worker.Spec{
		Kind:   "pool-fs",
		Binary: exe,
		Count:  1,
		Env: map[string]string{
			"DIGITORN_WORKER_MODULES":           "filesystem",
			"DIGITORN_MODULE_FILESYSTEM_CONFIG": cfg,
		},
		StartTimeout: 10 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	pickCtx, pickCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pickCancel()
	c, err := m.Pick(pickCtx, "pool-fs")
	if err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(map[string]any{"path": "hello.txt"})
	resp := invoke(t, c, &service.InvokeRequest{
		ModuleID:  "filesystem",
		ToolName:  "read",
		Params:    params,
		RequestID: "roundtrip-1",
	})

	if !resp.Result.Success {
		t.Fatalf("Invoke unsuccessful : %+v", resp.Result)
	}
	if resp.RequestID != "roundtrip-1" {
		t.Errorf("RequestID lost across worker round-trip : %q", resp.RequestID)
	}
	if resp.DurationMs < 0 {
		t.Errorf("DurationMs negative : %d", resp.DurationMs)
	}
	// The filesystem module returns {content, bytes, ...} as Data.
	gotData, _ := json.Marshal(resp.Result.Data)
	if !contains([]string{string(gotData)}, "digitorn-worker-live") {
		t.Errorf("expected file content in Result.Data ; got %s", gotData)
	}
}

// TestWorker_InvokeUnknownModule_ReturnsErrorResult : an Invoke for a
// module the worker doesn't host returns Result.Success=false, never
// a gRPC error (so the daemon doesn't retry a config bug).
func TestWorker_InvokeUnknownModule_ReturnsErrorResult(t *testing.T) {
	_, pick := spawnPool(t, "pool-bad", "filesystem", 1)
	resp := invoke(t, pick(), &service.InvokeRequest{
		ModuleID: "this-module-is-not-hosted",
		ToolName: "whatever",
	})
	if resp.Result.Success {
		t.Errorf("Result.Success should be false for unknown module")
	}
	if resp.Result.Error == "" {
		t.Error("Result.Error empty — caller has no info")
	}
}

// TestWorker_ConcurrentInvokes : 100 concurrent Invokes against one
// worker, all succeed. Catches contention bugs in the moduleService
// wrapper.
func TestWorker_ConcurrentInvokes(t *testing.T) {
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("ok"), 0o644)

	exe := buildWorkerBinary(t)
	m := worker.NewManager(quietLogger())
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := `{"workspace":"` + jsonEscapePath(tmp) + `"}`
	if err := m.Spawn(ctx, worker.Spec{
		Kind: "pool-concurrent", Binary: exe, Count: 1,
		Env: map[string]string{
			"DIGITORN_WORKER_MODULES":           "filesystem",
			"DIGITORN_MODULE_FILESYSTEM_CONFIG": cfg,
		},
		StartTimeout: 10 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	pickCtx, pickCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pickCancel()
	c, err := m.Pick(pickCtx, "pool-concurrent")
	if err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(map[string]any{"path": "f.txt"})
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out := new(service.InvokeResponse)
			err := c.GRPC().Invoke(ctx,
				"/"+service.ServiceName+"/"+service.MethodInvoke,
				&service.InvokeRequest{ModuleID: "filesystem", ToolName: "read", Params: params},
				out,
				grpc.CallContentSubtype(service.CodecName),
			)
			if err != nil {
				errs <- err.Error()
				return
			}
			if !out.Result.Success {
				errs <- out.Result.Error
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent call failed : %s", e)
	}
}

// TestWorker_LoadBalance_RoundRobin : 3 workers in the pool ; 30
// Manifests calls hit every WorkerID at least once (the round-robin
// is generic to worker.Manager — this just sanity-checks that the
// daemon-side dispatch sees distinct workers).
func TestWorker_LoadBalance_RoundRobin(t *testing.T) {
	m, _ := spawnPool(t, "pool-rr", "filesystem", 3)

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := m.Pick(ctx, "pool-rr")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		resp := manifests(t, c)
		seen[resp.WorkerID]++
	}
	if len(seen) != 3 {
		t.Fatalf("expected to see all 3 workers, got %d : %v", len(seen), seen)
	}
}

// ----- small helpers -----

func manifestIDs(in []domainmodule.Manifest) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.ID
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if len(needle) <= len(s) {
			for i := 0; i+len(needle) <= len(s); i++ {
				if s[i:i+len(needle)] == needle {
					return true
				}
			}
		}
	}
	return false
}

// jsonEscapePath escapes a filesystem path for embedding inside a
// JSON literal (Windows paths contain backslashes that JSON treats
// as escape sequences).
func jsonEscapePath(p string) string {
	b, _ := json.Marshal(p)
	// json.Marshal wraps the string in quotes — drop them so we
	// concatenate into our parent literal cleanly.
	return string(b[1 : len(b)-1])
}
