//go:build mcpintegration

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/digitornai/digitorn/pkg/module"
)

// TestSoak_SustainedLoad hammers the MCP module's hot paths — connect/dispatch/
// result-wrapping + periodic forced reconnect across multiple servers — under
// sustained concurrency, sampling goroutine count + heap to catch leaks. Default
// 60s; override for a real soak:
//
//	MCP_SOAK_DURATION=30m go test -tags mcpintegration -run TestSoak_SustainedLoad ./internal/modules/mcp/ -v -timeout 12h
func TestSoak_SustainedLoad(t *testing.T) {
	dur := soakDuration()
	const concurrency = 32

	// Two reliable in-process servers (no external deps) so the soak measures
	// OUR code, not a flaky remote.
	var servers map[string]any
	var tools []string
	var argKey string
	if os.Getenv("MCP_SOAK_STDIO") == "1" {
		// The common case: a real stdio server (one subprocess, pipe transport).
		servers = map[string]any{"a": map[string]any{
			"transport": "stdio", "command": "npx",
			"args":    []any{"-y", "@modelcontextprotocol/server-everything"},
			"sandbox": map[string]any{"permissions": []any{"process.exec"}},
		}}
		tools = []string{"mcp_a__echo"}
		argKey = "message" // server-everything echo param
	} else {
		argKey = "text" // newEchoServer param
		// streamable_http path (remote servers). ONE server instance per endpoint
		// (a fresh server per HTTP request would break the streamable session).
		echoA, echoB := newEchoServer(), newEchoServer()
		srvA := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
			func(*http.Request) *mcpsdk.Server { return echoA }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
		defer srvA.Close()
		srvB := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
			func(*http.Request) *mcpsdk.Server { return echoB }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
		defer srvB.Close()
		servers = map[string]any{
			"a": map[string]any{"transport": "streamable_http", "url": srvA.URL},
			"b": map[string]any{"transport": "streamable_http", "url": srvB.URL},
		}
		tools = []string{"mcp_a__echo", "mcp_b__echo"}
	}
	ctx := module.WithModuleConfig(context.Background(), map[string]any{"servers": servers})
	m := New()
	defer m.pool.shutdown(context.Background())
	if specs := m.LiveTools(ctx); len(specs) == 0 {
		t.Fatal("warm-up materialized nothing")
	}

	// Baseline AFTER warm-up + a GC, so pool/session goroutines are counted in.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	baseG := runtime.NumGoroutine()
	baseHeap := heapMB()
	t.Logf("soak: duration=%s concurrency=%d  baseline goroutines=%d heap=%dMB", dur, concurrency, baseG, baseHeap)

	var calls, errs int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Load: N workers alternating the two servers + a couple of tools.
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				tool := tools[n%len(tools)]
				res, err := m.Invoke(ctx, tool, []byte(fmt.Sprintf(`{%q:"soak-%d-%d"}`, argKey, id, n)))
				atomic.AddInt64(&calls, 1)
				if err != nil || !res.Success {
					atomic.AddInt64(&errs, 1)
				}
				n++
			}
		}(i)
	}

	// Reconnect stressor: every 15s force-reconnect both servers (exercises the
	// swap/close path concurrently with live traffic). Disable with
	// MCP_SOAK_RECONNECT=0 to isolate a per-call vs reconnect leak.
	if os.Getenv("MCP_SOAK_RECONNECT") != "0" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := time.NewTicker(15 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					_, _ = m.pool.reconnect(context.Background(), "a")
					_, _ = m.pool.reconnect(context.Background(), "b")
				}
			}
		}()
	}

	// Sampler: every 10s log goroutines/heap/calls so a leak shows as a trend.
	deadline := time.Now().Add(dur)
	samp := time.NewTicker(10 * time.Second)
	maxG := baseG
	for time.Now().Before(deadline) {
		select {
		case <-samp.C:
			g := runtime.NumGoroutine()
			if g > maxG {
				maxG = g
			}
			t.Logf("  +%4.0fs  calls=%-9d errs=%-4d goroutines=%-4d heap=%dMB",
				time.Since(deadline.Add(-dur)).Seconds(), atomic.LoadInt64(&calls), atomic.LoadInt64(&errs), g, heapMB())
		case <-time.After(time.Until(deadline)):
		}
	}
	samp.Stop()
	close(stop)
	wg.Wait()

	// Final: let things settle + GC, then check goroutines/heap returned near
	// baseline (no unbounded growth) and the error rate is low.
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	finalG := runtime.NumGoroutine()
	finalHeap := heapMB()
	c, e := atomic.LoadInt64(&calls), atomic.LoadInt64(&errs)
	t.Logf("soak DONE: calls=%d errs=%d  goroutines base=%d peak=%d final=%d  heap base=%dMB final=%dMB",
		c, e, baseG, maxG, finalG, baseHeap, finalHeap)

	if c == 0 {
		t.Fatal("no calls executed")
	}
	if rate := float64(e) / float64(c); rate > 0.02 {
		t.Errorf("error rate too high: %.2f%% (%d/%d)", rate*100, e, c)
	}
	// A LEAK shows as unbounded growth (the pre-fix http path hit thousands). A
	// bounded http connection pool (MaxConnsPerHost=64 × a few hosts, each with
	// reader/writer goroutines) is a legitimate working set, so allow generous
	// slack — a real leak blows far past this.
	if finalG > baseG+400 {
		t.Errorf("goroutine LEAK suspected: base=%d final=%d (peak=%d)", baseG, finalG, maxG)
	}
	// Heap must not blow up proportional to call count.
	if finalHeap > baseHeap+200 {
		t.Errorf("heap growth suspected: base=%dMB final=%dMB after %d calls", baseHeap, finalHeap, c)
	}
}

func heapMB() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc / (1024 * 1024))
}

func soakDuration() time.Duration {
	if v := os.Getenv("MCP_SOAK_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 60 * time.Second
}
