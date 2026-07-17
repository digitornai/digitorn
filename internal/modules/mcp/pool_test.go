package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeConn struct {
	id        string
	tools     []*mcpsdk.Tool
	prompts   []*mcpsdk.Prompt
	resources []*mcpsdk.Resource
	pingErr   error
	closed    bool
}

func (f *fakeConn) listTools(context.Context) ([]*mcpsdk.Tool, error) { return f.tools, nil }
func (f *fakeConn) listResources(context.Context) ([]*mcpsdk.Resource, error) {
	return f.resources, nil
}
func (f *fakeConn) listPrompts(context.Context) ([]*mcpsdk.Prompt, error) { return f.prompts, nil }
func (f *fakeConn) ping(context.Context) error                            { return f.pingErr }
func (f *fakeConn) close() error                                          { f.closed = true; return nil }
func (f *fakeConn) getPrompt(context.Context, string, map[string]string) (*mcpsdk.GetPromptResult, error) {
	return &mcpsdk.GetPromptResult{}, nil
}
func (f *fakeConn) readResource(context.Context, string) (*mcpsdk.ReadResourceResult, error) {
	return &mcpsdk.ReadResourceResult{}, nil
}
func (f *fakeConn) callTool(_ context.Context, name string, _ any) (*mcpsdk.CallToolResult, error) {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok:" + name}}}, nil
}

func toolList(names ...string) []*mcpsdk.Tool {
	out := make([]*mcpsdk.Tool, len(names))
	for i, n := range names {
		out[i] = &mcpsdk.Tool{Name: n}
	}
	return out
}

func fastPool(dial func(context.Context, connectSpec) (mcpConn, error)) *pool {
	p := newPool(2)
	p.base = time.Millisecond
	p.maxWait = 5 * time.Millisecond
	p.dialFn = dial
	return p
}

func TestPoolConnectCachesTools(t *testing.T) {
	fc := &fakeConn{tools: toolList("a", "b")}
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { return fc, nil })
	snap, err := p.connect(context.Background(), "srv", connectSpec{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if snap.Status != statusConnected || snap.Tools != 2 {
		t.Fatalf("want connected/2 tools, got %+v", snap)
	}
	if ent, _ := p.get("srv"); len(ent.tools) != 2 {
		t.Fatalf("tools not cached: %d", len(ent.tools))
	}
}

func TestPoolDialFailStoresErrorEntry(t *testing.T) {
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { return nil, errors.New("boom") })
	snap, err := p.connect(context.Background(), "srv", connectSpec{})
	if err == nil {
		t.Fatal("expected error")
	}
	if snap.Status != statusError {
		t.Fatalf("want error status, got %+v", snap)
	}
	if len(p.servers()) != 1 {
		t.Fatal("failed server must stay listable")
	}
}

func TestPoolConnectReplacesAndClosesOld(t *testing.T) {
	a := &fakeConn{id: "a"}
	b := &fakeConn{id: "b"}
	seq := []mcpConn{a, b}
	i := 0
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { c := seq[i]; i++; return c, nil })
	p.connect(context.Background(), "srv", connectSpec{})
	p.connect(context.Background(), "srv", connectSpec{})
	if !a.closed {
		t.Fatal("old connection must be closed on reconnect-via-connect")
	}
	if ent, _ := p.get("srv"); ent.conn != b {
		t.Fatal("new connection must be installed")
	}
}

func TestPoolDisconnectIdempotent(t *testing.T) {
	fc := &fakeConn{}
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { return fc, nil })
	if snap := p.disconnect(context.Background(), "ghost"); snap.Status != statusDisconnected {
		t.Fatal("disconnect of unknown must be a no-op success")
	}
	p.connect(context.Background(), "srv", connectSpec{})
	p.disconnect(context.Background(), "srv")
	if !fc.closed {
		t.Fatal("disconnect must close the connection")
	}
	if _, ok := p.get("srv"); ok {
		t.Fatal("entry must be removed")
	}
}

func TestPoolCallToolGuard(t *testing.T) {
	fc := &fakeConn{}
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { return fc, nil })
	if _, err := p.callTool(context.Background(), "srv", "x", nil); err == nil {
		t.Fatal("call on not-connected server must error")
	}
	p.connect(context.Background(), "srv", connectSpec{})
	res, err := p.callTool(context.Background(), "srv", "echo", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if tc, ok := res.Content[0].(*mcpsdk.TextContent); !ok || tc.Text != "ok:echo" {
		t.Fatalf("unexpected result: %+v", res.Content)
	}
}

func TestPoolReconnectSwaps(t *testing.T) {
	a := &fakeConn{}
	b := &fakeConn{tools: toolList("t")}
	seq := []mcpConn{a, b}
	i := 0
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { c := seq[i]; i++; return c, nil })
	p.connect(context.Background(), "srv", connectSpec{})
	snap, err := p.reconnect(context.Background(), "srv")
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !a.closed {
		t.Fatal("old connection must be closed after swap")
	}
	if ent, _ := p.get("srv"); ent.conn != b || snap.Tools != 1 {
		t.Fatalf("new connection must be swapped in, got %+v", snap)
	}
}

func TestPoolReconnectRetriesThenFails(t *testing.T) {
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { return nil, errors.New("down") })
	ok := &fakeConn{}
	p.dialFn = func(context.Context, connectSpec) (mcpConn, error) { return ok, nil }
	p.connect(context.Background(), "srv", connectSpec{})
	p.dialFn = func(context.Context, connectSpec) (mcpConn, error) { return nil, errors.New("down") }
	snap, err := p.reconnect(context.Background(), "srv")
	if err == nil {
		t.Fatal("expected reconnect failure")
	}
	if snap.Status != statusError {
		t.Fatalf("want error status, got %+v", snap)
	}
	if _, ok := p.get("srv"); !ok {
		t.Fatal("old entry must be preserved on reconnect failure")
	}
}

func TestPoolHealthCheck(t *testing.T) {
	fc := &fakeConn{}
	p := fastPool(func(context.Context, connectSpec) (mcpConn, error) { return fc, nil })
	p.connect(context.Background(), "srv", connectSpec{})
	if failed := p.healthCheck(context.Background()); len(failed) != 0 {
		t.Fatalf("healthy server must not be reported, got %v", failed)
	}
	fc.pingErr = errors.New("dead")
	failed := p.healthCheck(context.Background())
	if len(failed) != 1 || failed[0] != "srv" {
		t.Fatalf("failed ping must be reported, got %v", failed)
	}
	if ent, _ := p.get("srv"); ent.status != statusError {
		t.Fatal("failed ping must mark status error")
	}
}
