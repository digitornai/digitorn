//go:build mcpintegration

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDialStdioReal proves the SDK wrapper end-to-end against a real MCP server:
// handshake, tools/list, tools/call, content mapping, ping. Gated behind the
// mcpintegration tag (needs npx + network):
//
//	go test -tags mcpintegration -run TestDialStdioReal ./internal/modules/mcp/ -v
func TestDialStdioReal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c, err := dial(ctx, connectSpec{
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-everything"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()

	tools, err := c.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected at least one tool")
	}
	t.Logf("discovered %d tools", len(tools))
	has := false
	for _, tl := range tools {
		if tl.Name == "echo" {
			has = true
		}
	}
	if !has {
		t.Fatalf("expected an 'echo' tool among %d tools", len(tools))
	}

	res, err := c.callTool(ctx, "echo", map[string]any{"message": "digitorn-mcp-ok"})
	if err != nil {
		t.Fatalf("callTool echo: %v", err)
	}
	if res.IsError {
		t.Fatal("echo returned IsError")
	}
	var text strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcpsdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "digitorn-mcp-ok") {
		t.Fatalf("echo result missing payload, got %q", text.String())
	}

	if err := c.ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}
