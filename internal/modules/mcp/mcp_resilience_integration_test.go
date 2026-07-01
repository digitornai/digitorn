//go:build mcpintegration

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/digitornai/digitorn/pkg/module"
)

// TestConcurrency_ParallelCalls hammers ONE live MCP connection with many
// concurrent tool calls. Run with -race it proves the pool's call path is
// thread-safe (the RWMutex guarding the connection map + the SDK session under
// concurrent CallTool), which is the floor for "stable under load".
//
//	go test -tags mcpintegration -race -run TestConcurrency ./internal/modules/mcp/ -v
func TestConcurrency_ParallelCalls(t *testing.T) {
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return newEchoServer() },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true},
	))
	defer srv.Close()

	ctx := module.WithModuleConfig(context.Background(), map[string]any{
		"servers": map[string]any{"srv": map[string]any{"transport": "streamable_http", "url": srv.URL}},
	})
	m := New()
	defer m.pool.shutdown(context.Background())

	// Warm the connection once so the parallel storm exercises concurrent CALLS,
	// not concurrent connects.
	if specs := m.LiveTools(ctx); len(specs) == 0 {
		t.Fatal("no tools materialized")
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("c%d", i)
			res, err := m.Invoke(ctx, "mcp_srv__echo", []byte(fmt.Sprintf(`{"text":%q}`, want)))
			if err != nil || !res.Success {
				errs <- fmt.Errorf("call %d: err=%v res=%+v", i, err, res)
				return
			}
			data, _ := res.Data.(map[string]any)
			if out, _ := data["output"].(string); !strings.Contains(out, "echo:"+want) {
				errs <- fmt.Errorf("call %d: wrong output %q", i, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestResilience_ReconnectAfterDrop proves the pool recovers a connection after
// the underlying server drops: connect → kill the server → a reconnect to a
// fresh server on the SAME url succeeds. Models a server restart / transient
// outage (the health loop drives reconnect in production).
func TestResilience_ReconnectAfterDrop(t *testing.T) {
	// A switchable backend so we can "kill" then "restore" the server behind a
	// stable URL (httptest gives a fixed port for the listener's lifetime).
	var mu sync.Mutex
	alive := true
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return newEchoServer() },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := alive
		mu.Unlock()
		if !ok {
			http.Error(w, "server down", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	p := newPool(4)
	defer p.shutdown(context.Background())
	ctx := context.Background()
	spec := connectSpec{Transport: "streamable_http", URL: srv.URL, Timeout: 10 * time.Second}

	if _, err := p.connect(ctx, "srv", spec); err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	// Drop the backend, then reconnect: with a transient 503 the pool retries and,
	// once we restore the backend mid-backoff, recovers.
	mu.Lock()
	alive = false
	mu.Unlock()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		mu.Lock()
		alive = true
		mu.Unlock()
	}()
	snap, err := p.reconnect(ctx, "srv")
	if err != nil {
		t.Fatalf("reconnect did not recover after the backend came back: %v (snap=%+v)", err, snap)
	}
	if snap.Status != statusConnected {
		t.Fatalf("expected reconnected status, got %q", snap.Status)
	}
}
