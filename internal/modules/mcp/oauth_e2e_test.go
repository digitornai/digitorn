//go:build mcpintegration

// Real-stack MCP OAuth end-to-end proof. No mocks on the hot path: a REAL MCP
// server (official go-sdk) over REAL HTTP that REQUIRES a Bearer token from the
// initialize handshake onward, a REAL OAuth token exchange (a stand-in provider),
// and the REAL crypto/token/state stores. It exercises the whole loop:
//
//	discover → resolve(no token)=needs_auth → authorize → callback → exchange →
//	resolve(token)=AuthContext → tool call hits the auth-gated server → success.
//
// Run: go test -tags mcpintegration ./internal/modules/mcp/ -run OAuthE2E -v
package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	mcp "github.com/digitornai/digitorn/internal/modules/mcp"
	"github.com/digitornai/digitorn/internal/persistence/models"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

const realToken = "REALTOKEN-xyz"

type echoArgs struct {
	Text string `json:"text"`
}

// startMCPServer stands up a real go-sdk MCP server over HTTP that 401s any
// request (including initialize) whose Authorization is not "Bearer <want>".
// sawInitAuth flips true once a valid-auth request carrying method "initialize"
// is seen — proving the token reaches the handshake, not just tool calls.
func startMCPServer(t *testing.T, want string) (url string, sawInitAuth *atomic.Bool) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "e2e", Version: "v1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo back the input"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, a echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo:" + a.Text}}}, nil, nil
		})
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true},
	)

	sawInit := &atomic.Bool{}
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && peekMethod(r) == "initialize" {
			sawInit.Store(true)
		}
		handler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(gate)
	t.Cleanup(ts.Close)
	return ts.URL, sawInit
}

// peekMethod sniffs the jsonrpc "method" from a POST body, then restores the
// body so the real handler can read it (mirrors the SDK's example middleware).
func peekMethod(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &msg)
	return msg.Method
}

// startOAuthProvider stands up a token endpoint that returns realToken for any
// authorization_code exchange (a stand-in for google/github/etc).
func startOAuthProvider(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": realToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func newE2EService(t *testing.T) *mcpoauth.Service {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := gdb.AutoMigrate(&models.Credential{}, &models.OAuthState{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sealer, err := mcpoauth.NewSealer(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return mcpoauth.NewService(gdb, sealer)
}

func TestOAuthE2E_FullLoop(t *testing.T) {
	mcpURL, sawInitAuth := startMCPServer(t, realToken)
	providerURL := startOAuthProvider(t)

	authCfg := &schema.MCPAuthConfig{
		Type:         "oauth2",
		Provider:     "custom",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthorizeURL: providerURL + "/authorize",
		TokenURL:     providerURL + "/token",
		RedirectURI:  "https://daemon.example/callback",
	}
	svc := newE2EService(t)
	svc.SetServerAuthLookup(func(_, _ string) *schema.MCPAuthConfig { return authCfg })

	ctx := context.Background()
	const userID = "alice"

	// 1) DISCOVER → RESOLVE with no token ⇒ needs_auth challenge.
	ac, ch, err := svc.ResolveAuth(ctx, userID, "app", "mcp", "mcp_srv__echo")
	if err != nil {
		t.Fatalf("resolve#1: %v", err)
	}
	if ac != nil || ch == nil {
		t.Fatalf("expected a challenge, got ac=%+v ch=%+v", ac, ch)
	}
	if ch.State == "" || !strings.HasPrefix(ch.AuthURL, providerURL+"/authorize") {
		t.Fatalf("bad challenge: %+v", ch)
	}

	// 2) CALLBACK: consume the state (single-use) and exchange the code → token stored.
	p, err := svc.TakeState(ctx, ch.State)
	if err != nil || p == nil {
		t.Fatalf("take state: p=%v err=%v", p, err)
	}
	if p.UserID != userID || p.ServerID != "srv" {
		t.Fatalf("state not bound to user/server: %+v", p)
	}
	if err := svc.Exchange(ctx, authCfg, p, "auth-code-123"); err != nil {
		t.Fatalf("exchange: %v", err)
	}

	// 3) RESOLVE again ⇒ a valid AuthContext carrying the real token.
	ac, ch, err = svc.ResolveAuth(ctx, userID, "app", "mcp", "mcp_srv__echo")
	if err != nil {
		t.Fatalf("resolve#2: %v", err)
	}
	if ch != nil || ac == nil {
		t.Fatalf("expected an auth context, got ac=%+v ch=%+v", ac, ch)
	}
	if ac.Token != realToken {
		t.Fatalf("resolved token = %q, want %q", ac.Token, realToken)
	}

	// 4) TOOL CALL through the real module → the auth-gated real MCP server.
	m := mcp.New()
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	servers := map[string]any{"srv": map[string]any{"transport": "streamable_http", "url": mcpURL}}
	callCtx := tool.WithIdentity(ctx, tool.Identity{UserID: userID, ModuleID: "mcp"})
	callCtx = pkgmodule.WithModuleConfig(callCtx, map[string]any{"servers": servers})
	callCtx = pkgmodule.WithAuthContext(callCtx, pkgmodule.AuthContext{Token: ac.Token, TokenType: ac.TokenType})
	callCtx, cancel := context.WithTimeout(callCtx, 20*time.Second)
	defer cancel()

	res, err := m.Invoke(callCtx, "mcp_srv__echo", []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("invoke transport error: %v", err)
	}
	if !res.Success {
		t.Fatalf("tool call failed against auth-gated server: %+v", res)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data not a map: %T", res.Data)
	}
	if out, _ := data["output"].(string); !strings.Contains(out, "echo:hello") {
		t.Fatalf("unexpected tool output: %v", data["output"])
	}
	if data["_source"] != "mcp_server:srv" {
		t.Errorf("missing source envelope: %v", data["_source"])
	}
	// PROOF the token reached the initialize handshake, not just the tool call —
	// the per-call header injection extends to connect because ensureConnected
	// dials with the call context.
	if !sawInitAuth.Load() {
		t.Fatal("server never saw an authorized initialize — per-call injection did not cover the handshake")
	}
}

// TestOAuthE2E_NoTokenIsRejected proves the gate actually gates: without an
// AuthContext the connection is refused (401 at the handshake) and the tool fails.
func TestOAuthE2E_NoTokenIsRejected(t *testing.T) {
	mcpURL, _ := startMCPServer(t, realToken)
	m := mcp.New()
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	servers := map[string]any{"srv": map[string]any{"transport": "streamable_http", "url": mcpURL}}
	ctx := tool.WithIdentity(context.Background(), tool.Identity{UserID: "bob", ModuleID: "mcp"})
	ctx = pkgmodule.WithModuleConfig(ctx, map[string]any{"servers": servers})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	res, _ := m.Invoke(ctx, "mcp_srv__echo", []byte(`{"text":"hi"}`))
	if res.Success {
		t.Fatal("tool call succeeded WITHOUT a token against an auth-gated server")
	}
}
