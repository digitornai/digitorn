package middlewareplugin_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	mwp "github.com/mbathepaul/digitorn/internal/middlewareplugin"
	"github.com/mbathepaul/digitorn/internal/module/service"
	"github.com/mbathepaul/digitorn/internal/ports"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// host is a minimal service.Service hosting one module — what a worker binary
// does internally.
type host struct{ mod domainmodule.Module }

func (h *host) Invoke(ctx context.Context, req *service.InvokeRequest) (*service.InvokeResponse, error) {
	res, err := h.mod.Invoke(ctx, req.ToolName, req.Params)
	if err != nil {
		return nil, err
	}
	return &service.InvokeResponse{Result: res, RequestID: req.RequestID}, nil
}

func (h *host) Manifests(_ context.Context, _ *service.ManifestsRequest) (*service.ManifestsResponse, error) {
	return &service.ManifestsResponse{Modules: []domainmodule.Manifest{h.mod.Manifest()}}, nil
}

type fakeConn struct{ cc *grpc.ClientConn }

func (f fakeConn) GRPC() *grpc.ClientConn { return f.cc }
func (f fakeConn) Handle() worker.Handle  { return worker.Handle{} }
func (f fakeConn) Close() error           { return nil }

type fakePicker struct{ c worker.Conn }

func (p fakePicker) Pick(context.Context, worker.Kind) (worker.Conn, error) { return p.c, nil }

type errPicker struct{}

func (errPicker) Pick(context.Context, worker.Kind) (worker.Conn, error) {
	return nil, errors.New("no worker")
}

// startPlugin spins up an in-process gRPC server hosting the module and returns
// a Picker pointed at it (real gRPC over loopback, no spawned process).
func startPlugin(t *testing.T, mod domainmodule.Module) mwp.Picker {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	service.RegisterService(srv, &host{mod: mod})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	cc, err := grpc.NewClient("passthrough:///"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype(service.CodecName)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return fakePicker{c: fakeConn{cc: cc}}
}

func TestPlugin_BeforeMutatesContext(t *testing.T) {
	before := func(_ context.Context, c *mwp.Context) (string, bool, error) {
		c.SystemPrompt += " [INJECTED]"
		for i := range c.Messages {
			if c.Messages[i].Role == "user" {
				c.Messages[i].Content = "[masked]"
			}
		}
		return "", false, nil
	}
	picker := startPlugin(t, mwp.Module("test_mw", before, nil))
	p, err := mwp.New(mwp.Options{ModuleID: "test_mw", Kind: "k", Picker: picker, FailOpen: true})
	if err != nil {
		t.Fatal(err)
	}

	mctx := &ports.MiddlewareContext{
		SystemPrompt: "base",
		Messages:     []ports.LLMMessage{{Role: "user", Content: "secret data"}},
	}
	resp, sc, err := p.Before(context.Background(), mctx)
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if sc || resp != "" {
		t.Errorf("expected no short-circuit, got (%q, %v)", resp, sc)
	}
	if mctx.SystemPrompt != "base [INJECTED]" {
		t.Errorf("system prompt not mutated across the plugin boundary: %q", mctx.SystemPrompt)
	}
	if mctx.Messages[0].Content != "[masked]" {
		t.Errorf("message not mutated across the plugin boundary: %q", mctx.Messages[0].Content)
	}
}

func TestPlugin_BeforeShortCircuit(t *testing.T) {
	before := func(_ context.Context, _ *mwp.Context) (string, bool, error) {
		return "BLOCKED BY PLUGIN", true, nil
	}
	picker := startPlugin(t, mwp.Module("blk", before, nil))
	p, _ := mwp.New(mwp.Options{ModuleID: "blk", Kind: "k", Picker: picker})
	resp, sc, err := p.Before(context.Background(), &ports.MiddlewareContext{})
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if !sc || resp != "BLOCKED BY PLUGIN" {
		t.Errorf("plugin short-circuit not propagated: (%q, %v)", resp, sc)
	}
}

func TestPlugin_AfterTransforms(t *testing.T) {
	after := func(_ context.Context, _ *mwp.Context, response string, _ []mwp.ToolCall) (string, error) {
		return strings.ToUpper(response), nil
	}
	picker := startPlugin(t, mwp.Module("up", nil, after))
	p, _ := mwp.New(mwp.Options{ModuleID: "up", Kind: "k", Picker: picker})
	out, err := p.After(context.Background(), &ports.MiddlewareContext{}, "hello world", nil)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if out != "HELLO WORLD" {
		t.Errorf("plugin After transform not applied: %q", out)
	}
}

func TestPlugin_FailOpenVsFailClosed(t *testing.T) {
	// Fail-open : a transport error degrades gracefully (no mutation, no error).
	open, _ := mwp.New(mwp.Options{ModuleID: "x", Kind: "k", Picker: errPicker{}, FailOpen: true})
	if _, sc, err := open.Before(context.Background(), &ports.MiddlewareContext{SystemPrompt: "p"}); err != nil || sc {
		t.Errorf("fail-open Before must degrade silently, got (sc=%v, err=%v)", sc, err)
	}
	if out, err := open.After(context.Background(), &ports.MiddlewareContext{}, "keep", nil); err != nil || out != "keep" {
		t.Errorf("fail-open After must return the original response, got (%q, %v)", out, err)
	}
	// Fail-closed : a transport error fails the call.
	closed, _ := mwp.New(mwp.Options{ModuleID: "x", Kind: "k", Picker: errPicker{}, FailOpen: false})
	if _, _, err := closed.Before(context.Background(), &ports.MiddlewareContext{}); err == nil {
		t.Error("fail-closed Before must return the transport error")
	}
}
