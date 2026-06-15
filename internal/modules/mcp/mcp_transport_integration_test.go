//go:build mcpintegration

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mbathepaul/digitorn/pkg/module"
)

type echoArgs struct {
	Text string `json:"text"`
}

func newEchoServer() *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "transport-test", Version: "v1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo back the text"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, a echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo:" + a.Text}}}, nil, nil
		})
	return srv
}

// TestTransport_HTTP_and_SSE proves BOTH remote transports — streamable_http AND
// SSE — connect, list tools and call a tool through the module, using real
// in-process go-sdk servers. SSE was previously rejected ("phase 1.5"); this
// confirms the one-line SDK wiring works.
//
//	go test -tags mcpintegration -run TestTransport ./internal/modules/mcp/ -v
func TestTransport_HTTP_and_SSE(t *testing.T) {
	httpSrv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return newEchoServer() },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true},
	))
	defer httpSrv.Close()

	sseSrv := httptest.NewServer(mcpsdk.NewSSEHandler(
		func(*http.Request) *mcpsdk.Server { return newEchoServer() }, nil,
	))
	defer sseSrv.Close()

	cases := []struct{ transport, url string }{
		{"streamable_http", httpSrv.URL},
		{"http", httpSrv.URL}, // alias → streamable_http
		{"sse", sseSrv.URL},
	}
	for _, c := range cases {
		t.Run(c.transport, func(t *testing.T) {
			ctx := module.WithModuleConfig(context.Background(), map[string]any{
				"servers": map[string]any{
					"srv": map[string]any{"transport": c.transport, "url": c.url},
				},
			})
			m := New()
			defer m.pool.shutdown(context.Background())

			specs := m.LiveTools(ctx)
			if len(specs) == 0 {
				t.Fatalf("[%s] LiveTools materialized nothing", c.transport)
			}
			found := false
			for i := range specs {
				if specs[i].Name == "mcp_srv__echo" {
					found = true
				}
			}
			if !found {
				t.Fatalf("[%s] echo tool not materialized: %+v", c.transport, specs)
			}
			res, err := m.Invoke(ctx, "mcp_srv__echo", []byte(`{"text":"over-`+c.transport+`"}`))
			if err != nil || !res.Success {
				t.Fatalf("[%s] invoke failed: err=%v res=%+v", c.transport, err, res)
			}
			data, _ := res.Data.(map[string]any)
			if out, _ := data["output"].(string); !strings.Contains(out, "echo:over-"+c.transport) {
				t.Fatalf("[%s] wrong echo output: %q", c.transport, out)
			}
		})
	}
}
