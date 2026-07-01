package servicebus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/core/servicebus"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/modules/filesystem"
	"github.com/digitornai/digitorn/pkg/module"
)

// noopModule is a minimal in-process module used to measure DISPATCH
// cost without filesystem syscall overhead. Its single tool "ping"
// returns Success immediately. Lets us bench the bus + module SDK
// path itself, isolated from any I/O.
type noopModule struct {
	module.Base
}

func newNoopModule() *noopModule {
	m := &noopModule{}
	m.Base = module.Base{
		ID:      "noop",
		Version: "0.0.1",
	}
	m.RegisterTool(module.Tool{
		Name: "ping",
		Handler: func(ctx context.Context, params json.RawMessage) (tool.Result, error) {
			return tool.Result{Success: true}, nil
		},
	})
	return m
}

// startBusNoop boots a bus with the noop module wired in.
func startBusNoop(t testing.TB) *servicebus.Bus {
	t.Helper()
	bus := servicebus.New()
	m := newNoopModule()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bus.Register(m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return bus
}

// M-LIVE-1 + D6 : end-to-end dispatch through the ServiceBus.
//
// Question we answer here :
//   - Does the in-process module pattern work bout-en-bout ?
//   - Does the existing servicebus (sync.RWMutex) hold up at ≥100K
//     dispatches per second, p99 < 1ms ?
//
// If yes : foundation is solid, we can build the worker-bridge on top.
// If no  : we fix here before anything else.

// startBus boots a ServiceBus with the filesystem module initialized
// against a tmp workspace. Returns the bus + workspace path.
func startBus(t testing.TB) (*servicebus.Bus, string) {
	t.Helper()
	bus := servicebus.New()
	mod := filesystem.New()
	ws := t.TempDir()
	if err := mod.Init(context.Background(), map[string]any{
		"workspace":      ws,
		"max_file_bytes": 1024 * 1024,
	}); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := mod.Start(context.Background()); err != nil {
		t.Fatalf("module start: %v", err)
	}
	if err := bus.Register(mod); err != nil {
		t.Fatalf("bus register: %v", err)
	}
	t.Cleanup(func() { _ = mod.Stop(context.Background()) })
	return bus, ws
}

// TestLive_DispatchInProc_FilesystemRead proves end-to-end : a module
// loaded into the bus is invocable via bus.Call() exactly as the
// runtime will use it. Cover the happy path AND a not-found path.
func TestLive_DispatchInProc_FilesystemRead(t *testing.T) {
	bus, ws := startBus(t)

	// Write a real file the module will read.
	content := []byte("digitorn-live-test-content\n")
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(map[string]any{"path": "hello.txt"})
	res, err := bus.Call(context.Background(), "filesystem", "read", params)
	if err != nil {
		t.Fatalf("Call: %v\nresult: %+v", err, res)
	}
	if !res.Success {
		t.Fatalf("Result.Success=false : %+v", res)
	}
	// The filesystem module returns Data as a map with the content.
	got := fmt.Sprintf("%v", res.Data)
	if !contains(got, "digitorn-live-test-content") {
		t.Errorf("expected content in Data, got %v", res.Data)
	}
}

// TestLive_DispatchInProc_UnknownModule must surface a clean error,
// never panic.
func TestLive_DispatchInProc_UnknownModule(t *testing.T) {
	bus, _ := startBus(t)
	_, err := bus.Call(context.Background(), "ghost-module", "read", nil)
	if err == nil {
		t.Fatal("expected error for unknown module")
	}
}

// TestLive_DispatchInProc_UnknownTool : the module exists but the
// tool name is wrong. Must return a clean tool.Result error, not panic.
func TestLive_DispatchInProc_UnknownTool(t *testing.T) {
	bus, _ := startBus(t)
	res, err := bus.Call(context.Background(), "filesystem", "no-such-tool", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if res.Success {
		t.Error("Result.Success should be false")
	}
}

// BenchmarkLive_Dispatch measures p50/p99/throughput on the actual
// in-process dispatch path : json-marshal params, bus.Call, read the
// file, return Result. This is the realistic per-tool-call cost in
// the runtime hot path.
func BenchmarkLive_Dispatch(b *testing.B) {
	bus, ws := startBus(b)
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("bench"), 0o644); err != nil {
		b.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"path": "f.txt"})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, err := bus.Call(context.Background(), "filesystem", "read", params)
		if err != nil || !res.Success {
			b.Fatalf("call: err=%v ok=%v", err, res.Success)
		}
	}
}

// BenchmarkLive_DispatchParallel : same call under -benchparallel
// load to expose sync.RWMutex contention on many cores. p99 here is
// what matters at 10M sessions.
func BenchmarkLive_DispatchParallel(b *testing.B) {
	bus, ws := startBus(b)
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("bench"), 0o644); err != nil {
		b.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"path": "f.txt"})

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := bus.Call(context.Background(), "filesystem", "read", params)
			if err != nil || !res.Success {
				b.Fatalf("call: err=%v ok=%v", err, res.Success)
			}
		}
	})
}

// TestLive_Dispatch_LatencyP99 measures DISPATCH cost only (no I/O)
// via the noop module. 100K sequential calls, p50 + p99 + max. This
// is the lower bound — real modules add their own work on top, but
// the bus + sync.RWMutex + module SDK is what we care about here.
func TestLive_Dispatch_LatencyP99(t *testing.T) {
	bus := startBusNoop(t)

	const N = 100_000
	durs := make([]time.Duration, N)
	for i := 0; i < N; i++ {
		t0 := time.Now()
		res, err := bus.Call(context.Background(), "noop", "ping", nil)
		durs[i] = time.Since(t0)
		if err != nil || !res.Success {
			t.Fatalf("call %d: err=%v ok=%v", i, err, res.Success)
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p50 := durs[N/2]
	p99 := durs[(N*99)/100]
	pmax := durs[N-1]

	// Aggregate elapsed = wall clock of the whole loop ≈ sum of all
	// per-call times (we are single-goroutine here).
	var totalNs int64
	for _, d := range durs {
		totalNs += d.Nanoseconds()
	}
	totalSec := float64(totalNs) / float64(time.Second)
	throughput := float64(N) / totalSec

	t.Logf("100K dispatches (noop, single goroutine) : p50=%v p99=%v max=%v throughput=%.0f calls/sec",
		p50, p99, pmax, throughput)

	if p99 > 1*time.Millisecond {
		t.Errorf("p99=%v exceeds 1ms dispatch budget", p99)
	}
	if throughput < 100_000 {
		t.Errorf("throughput=%.0f calls/sec below 100K/sec target", throughput)
	}
}

// TestLive_Dispatch_ConcurrentLatency stresses the RWMutex : 16
// goroutines dispatching in parallel for 1 second, measure aggregate
// throughput. At 10M sessions this is the relevant number.
func TestLive_Dispatch_ConcurrentLatency(t *testing.T) {
	bus := startBusNoop(t)

	const goroutines = 16
	stop := make(chan struct{})
	var ops atomic.Uint64
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					res, err := bus.Call(context.Background(), "noop", "ping", nil)
					if err == nil && res.Success {
						ops.Add(1)
					}
				}
			}
		}()
	}

	const window = 1 * time.Second
	time.Sleep(window)
	close(stop)
	wg.Wait()

	total := ops.Load()
	throughput := float64(total) / window.Seconds()
	t.Logf("%d goroutines × %v → %d ops (%.0f ops/sec) on %d cores",
		goroutines, window, total, throughput, runtime.NumCPU())

	// 10M sessions × 1% actives × 1 tool/turn = 100K ops/sec target.
	// Concurrent we should be way above (closer to 1M ops/sec on 16 cores).
	if throughput < 500_000 {
		t.Errorf("concurrent throughput too low : %.0f ops/sec (want ≥ 500K)", throughput)
	}
}

// helper : substring check without pulling strings package twice.
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

// reference unused imports to keep the file compile-clean.
var _ = domainmodule.PlatformLinux
