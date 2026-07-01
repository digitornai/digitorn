package mcp

import (
	"context"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/digitornai/digitorn/pkg/module"
)

const (
	clientName    = "digitorn"
	clientVersion = "1.0.0"
)

// connectSpec is the resolved set of parameters to open one connection.
type connectSpec struct {
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	Timeout   time.Duration
	// AuthFP fingerprints the injected credential so the pool reconnects an
	// authenticated stdio server only when the credential actually changed
	// (covers env_token AND google_keyfile, where the token isn't in an env var).
	AuthFP string
}

// conn wraps one live MCP client session over the official SDK.
type conn struct {
	session *mcpsdk.ClientSession
}

func dial(ctx context.Context, spec connectSpec) (*conn, error) {
	tr, err := buildTransport(spec)
	if err != nil {
		return nil, err
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: clientName, Version: clientVersion}, nil)
	session, err := client.Connect(ctx, tr, nil)
	if err != nil {
		return nil, classify(err)
	}
	return &conn{session: session}, nil
}

func buildTransport(spec connectSpec) (mcpsdk.Transport, error) {
	switch normTransport(spec.Transport) {
	case "stdio":
		if spec.Command == "" {
			return nil, transportErr("stdio transport requires a command")
		}
		command, args := wrapPython(spec.Command, spec.Args)
		cmd := exec.Command(command, args...)
		cmd.Env = buildSafeEnv(spec.Env)
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	case "streamable_http":
		if spec.URL == "" {
			return nil, transportErr("http transport requires a url")
		}
		// Always install the header client: besides static headers, its
		// RoundTrip injects the daemon-resolved OAuth token from the per-call
		// context (one shared connection serves all users, token per request).
		t := &mcpsdk.StreamableClientTransport{Endpoint: spec.URL, HTTPClient: headerClient(spec.Headers)}
		return t, nil
	case "sse":
		// Legacy HTTP+SSE transport (2024-11-05 spec). The official SDK provides
		// the client; we just install the same header/OAuth-injecting http client.
		if spec.URL == "" {
			return nil, transportErr("sse transport requires a url")
		}
		return &mcpsdk.SSEClientTransport{Endpoint: spec.URL, HTTPClient: headerClient(spec.Headers)}, nil
	default:
		return nil, transportErr("unknown transport %q", spec.Transport)
	}
}

func normTransport(t string) string {
	switch t {
	case "", "stdio":
		return "stdio"
	case "http", "streamable_http":
		return "streamable_http"
	case "sse":
		return "sse"
	default:
		return t
	}
}

func (c *conn) listTools(ctx context.Context) ([]*mcpsdk.Tool, error) {
	res, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, classify(err)
	}
	return res.Tools, nil
}

func (c *conn) callTool(ctx context.Context, name string, args any) (*mcpsdk.CallToolResult, error) {
	res, err := c.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, classify(err)
	}
	return res, nil
}

func (c *conn) listResources(ctx context.Context) ([]*mcpsdk.Resource, error) {
	res, err := c.session.ListResources(ctx, nil)
	if err != nil {
		return nil, classify(err)
	}
	return res.Resources, nil
}

func (c *conn) listPrompts(ctx context.Context) ([]*mcpsdk.Prompt, error) {
	res, err := c.session.ListPrompts(ctx, nil)
	if err != nil {
		return nil, classify(err)
	}
	return res.Prompts, nil
}

func (c *conn) getPrompt(ctx context.Context, name string, args map[string]string) (*mcpsdk.GetPromptResult, error) {
	res, err := c.session.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return nil, classify(err)
	}
	return res, nil
}

func (c *conn) readResource(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error) {
	res, err := c.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, classify(err)
	}
	return res, nil
}

func (c *conn) ping(ctx context.Context) error {
	return classify(c.session.Ping(ctx, nil))
}

func (c *conn) close() error {
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}

// mcpHTTPTransport is the SHARED, bounded transport for all http/sse MCP
// connections. Unlike http.DefaultTransport it caps concurrent connections per
// host (MaxConnsPerHost) so a server that doesn't let connections return to the
// keep-alive pool produces BACK-PRESSURE instead of an unbounded dial leak (a
// soak found DefaultTransport piling up tens of thousands of dial goroutines
// under sustained streamable_http load). Higher MaxIdleConnsPerHost keeps more
// connections reusable; IdleConnTimeout reaps stragglers.
var mcpHTTPTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   16,
	MaxConnsPerHost:       64,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// headerClient injects static headers on every request — the point for
// daemon-resolved OAuth / configured bearer tokens on http transports.
func headerClient(headers map[string]string) *http.Client {
	return &http.Client{Transport: &headerRoundTripper{base: mcpHTTPTransport, headers: headers}}
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	// Per-call OAuth: overlay the Authorization header from the daemon-resolved
	// credential on this request's context (overrides any static one).
	if ac, ok := module.AuthContextFrom(req.Context()); ok && ac.Token != "" {
		// Normalize the scheme to the canonical "Bearer": providers return
		// token_type as "bearer" (lower-case) and some resource servers (e.g.
		// Notion's MCP endpoint) reject a lower-case scheme as an invalid token.
		scheme := ac.TokenType
		if scheme == "" || strings.EqualFold(scheme, "bearer") {
			scheme = "Bearer"
		}
		req.Header.Set("Authorization", scheme+" "+ac.Token)
	}
	return h.base.RoundTrip(req)
}
