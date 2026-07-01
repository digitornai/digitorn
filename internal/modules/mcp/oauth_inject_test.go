package mcp

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

type captureRT struct{ last *http.Request }

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.last = r
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestHeaderRoundTripper_InjectsTokenFromContext(t *testing.T) {
	cap := &captureRT{}
	h := &headerRoundTripper{base: cap, headers: map[string]string{"X-Static": "1"}}

	ctx := module.WithAuthContext(context.Background(), module.AuthContext{Token: "AT", TokenType: "Bearer"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://x", nil)
	if _, err := h.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := cap.last.Header.Get("Authorization"); got != "Bearer AT" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer AT")
	}
	if cap.last.Header.Get("X-Static") != "1" {
		t.Fatal("static header dropped")
	}
}

func TestHeaderRoundTripper_DefaultSchemeAndNoToken(t *testing.T) {
	cap := &captureRT{}
	h := &headerRoundTripper{base: cap, headers: nil}

	// Empty token type → defaults to Bearer.
	ctx := module.WithAuthContext(context.Background(), module.AuthContext{Token: "AT"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://x", nil)
	_, _ = h.RoundTrip(req)
	if got := cap.last.Header.Get("Authorization"); got != "Bearer AT" {
		t.Fatalf("default scheme: got %q", got)
	}

	// No auth context → no Authorization header at all.
	req2, _ := http.NewRequest(http.MethodGet, "https://x", nil)
	_, _ = h.RoundTrip(req2)
	if cap.last.Header.Get("Authorization") != "" {
		t.Fatal("Authorization set without an auth context")
	}
}

func oauthStdioServer(envVar string) schema.MCPServerConfig {
	return schema.MCPServerConfig{
		Transport: "stdio", Command: "x",
		Auth: &schema.MCPAuthConfig{Type: "oauth2", Provider: "custom", EnvTokenVar: envVar},
	}
}

func TestPhysicalKey(t *testing.T) {
	httpSrv := schema.MCPServerConfig{Transport: "streamable_http", URL: "https://x"}
	stdioPlain := schema.MCPServerConfig{Transport: "stdio", Command: "x"}
	stdioAuth := oauthStdioServer("TOK")

	if k := physicalKey("s", httpSrv, "userA"); k != "s" {
		t.Errorf("http must be shared, got %q", k)
	}
	if k := physicalKey("s", stdioPlain, "userA"); k != "s" {
		t.Errorf("non-auth stdio must be shared, got %q", k)
	}
	if k := physicalKey("s", stdioAuth, ""); k != "s" {
		t.Errorf("no user → shared, got %q", k)
	}
	if k := physicalKey("s", stdioAuth, "userA"); k != "s"+userKeySep+"userA" {
		t.Errorf("auth stdio must be per-user, got %q", k)
	}
}

// callCtx builds the worker-side call context: identity (userID) + per-app config
// + the resolved auth context.
func callCtx(userID, envVar, token string, servers map[string]any) context.Context {
	ctx := tool.WithIdentity(context.Background(), tool.Identity{UserID: userID, ModuleID: "mcp"})
	ctx = module.WithModuleConfig(ctx, map[string]any{"servers": servers})
	if token != "" {
		ctx = module.WithAuthContext(ctx, module.AuthContext{Token: token, EnvTokenVar: envVar})
	}
	return ctx
}

func TestEnsureConnected_PerUserStdioIsolation(t *testing.T) {
	m := New()
	m.pool.dialFn = func(_ context.Context, _ connectSpec) (mcpConn, error) { return &fakeConn{}, nil }

	servers := map[string]any{"srv": map[string]any{
		"transport": "stdio", "command": "x",
		"auth": map[string]any{"type": "oauth2", "provider": "custom", "env_token_var": "TOK"},
	}}

	m.ensureConnected(callCtx("userA", "TOK", "tokenA", servers), "")
	m.ensureConnected(callCtx("userB", "TOK", "tokenB", servers), "")

	entA, okA := m.pool.get("srv" + userKeySep + "userA")
	entB, okB := m.pool.get("srv" + userKeySep + "userB")
	if !okA || !okB {
		t.Fatalf("expected per-user entries, got A=%v B=%v", okA, okB)
	}
	if _, shared := m.pool.get("srv"); shared {
		t.Fatal("auth stdio must not create a shared 'srv' entry")
	}
	if entA.spec.Env["TOK"] != "tokenA" || entB.spec.Env["TOK"] != "tokenB" {
		t.Fatalf("token not injected per user: A=%q B=%q", entA.spec.Env["TOK"], entB.spec.Env["TOK"])
	}
}

func TestEnsureConnected_ReconnectsOnTokenChange(t *testing.T) {
	m := New()
	dials := 0
	m.pool.dialFn = func(_ context.Context, _ connectSpec) (mcpConn, error) { dials++; return &fakeConn{}, nil }
	servers := map[string]any{"srv": map[string]any{
		"transport": "stdio", "command": "x",
		"auth": map[string]any{"type": "oauth2", "provider": "custom", "env_token_var": "TOK"},
	}}

	m.ensureConnected(callCtx("userA", "TOK", "tok1", servers), "")
	m.ensureConnected(callCtx("userA", "TOK", "tok1", servers), "") // unchanged → no redial
	if dials != 1 {
		t.Fatalf("expected 1 dial for unchanged token, got %d", dials)
	}
	m.ensureConnected(callCtx("userA", "TOK", "tok2", servers), "") // rotated → redial
	if dials != 2 {
		t.Fatalf("expected redial on token change, got %d dials", dials)
	}
}

func TestEnsureConnected_HttpSharedAcrossUsers(t *testing.T) {
	m := New()
	m.pool.dialFn = func(_ context.Context, _ connectSpec) (mcpConn, error) { return &fakeConn{}, nil }
	servers := map[string]any{"web": map[string]any{"transport": "streamable_http", "url": "https://x"}}

	m.ensureConnected(callCtx("userA", "", "", servers), "")
	m.ensureConnected(callCtx("userB", "", "", servers), "")
	if _, ok := m.pool.get("web"); !ok {
		t.Fatal("http server should be a single shared entry")
	}
	if len(m.pool.servers()) != 1 {
		t.Fatalf("expected exactly 1 shared http entry, got %d", len(m.pool.servers()))
	}
}

func TestEvictOldestUserConn(t *testing.T) {
	p := newPool(2)
	base := time.Now()
	for i, id := range []string{"s\x00u1", "s\x00u2", "s\x00u3"} {
		p.putEntry(&serverEntry{id: id, conn: &fakeConn{}, status: statusConnected, createdAt: base.Add(time.Duration(i) * time.Second)})
	}
	// Keep at most 2 → evicting before adding the 4th removes the oldest (u1).
	p.evictOldestUserConn(userKeySep, 3)
	if _, ok := p.get("s\x00u1"); ok {
		t.Fatal("oldest per-user conn should have been evicted")
	}
	if _, ok := p.get("s\x00u2"); !ok {
		t.Fatal("newer conn must survive")
	}
}
