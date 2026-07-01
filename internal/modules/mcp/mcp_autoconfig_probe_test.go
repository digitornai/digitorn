//go:build mcpintegration

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// TestAutoConfig_ProbeRegistry hits the REAL MCP registry for a set of candidate
// server ids and logs what each resolves to — used to pick a reliable no-auth
// npm server for the full auto-config E2E.
//
//	go test -tags mcpintegration -run TestAutoConfig_ProbeRegistry ./internal/modules/mcp/ -v
func TestAutoConfig_ProbeRegistry(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{
		"everything", "sequentialthinking", "sequential-thinking", "memory",
		"time", "fetch", "git", "filesystem", "wikipedia", "calculator",
	} {
		srv, ok := searchRegistry(ctx, id)
		if !ok || srv == nil {
			t.Logf("%-22s -> (no registry hit)", id)
			continue
		}
		spec, det, mapped := registryToConnectSpec(srv, schema.MCPServerConfig{})
		if !mapped {
			t.Logf("%-22s -> not mappable", id)
			continue
		}
		auth := "none"
		if det != nil {
			auth = "oauth:" + det.Provider
		}
		// Actually CONNECT to the resolved real server and list its real tools.
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		c, err := dial(cctx, spec)
		if err != nil {
			cancel()
			t.Logf("%-22s -> %s/%s  DIAL-FAIL: %v", id, spec.Transport, auth, err)
			continue
		}
		tools, terr := c.listTools(cctx)
		_ = c.close()
		cancel()
		if terr != nil {
			t.Logf("%-22s -> %s/%s  LIST-FAIL: %v", id, spec.Transport, auth, terr)
			continue
		}
		names := make([]string, 0, len(tools))
		for _, tl := range tools {
			if tl != nil {
				names = append(names, tl.Name)
			}
		}
		if len(names) > 6 {
			names = names[:6]
		}
		t.Logf("%-22s -> CONNECTED %s/%s  %d tools: %s  (cmd=%s %s url=%s)",
			id, spec.Transport, auth, len(tools), strings.Join(names, ","),
			spec.Command, strings.Join(spec.Args, " "), spec.URL)
	}
}
