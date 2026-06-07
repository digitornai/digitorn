package mcp

import (
	"context"
	"net/http"
	"os/exec"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mbathepaul/digitorn/pkg/module"
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
		cmd := exec.Command(spec.Command, spec.Args...)
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
		return nil, transportErr("sse transport is not supported yet (phase 1.5)")
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

// headerClient injects static headers on every request — the point for
// daemon-resolved OAuth / configured bearer tokens on http transports.
func headerClient(headers map[string]string) *http.Client {
	return &http.Client{Transport: &headerRoundTripper{base: http.DefaultTransport, headers: headers}}
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
		scheme := ac.TokenType
		if scheme == "" {
			scheme = "Bearer"
		}
		req.Header.Set("Authorization", scheme+" "+ac.Token)
	}
	return h.base.RoundTrip(req)
}
