//go:build mcpintegration

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/pkg/module"
)

// TestDynamicAuth_AnyServer_E2E is the proof that the auth wiring is fully
// DYNAMIC — no per-server code, any shape. For each case a server the daemon has
// never seen (it exists ONLY in the registry) is declared with nothing but a
// credential in YAML, under an ARBITRARY shorthand that deliberately does NOT
// match the server's real env-var name. The daemon resolves it from the registry,
// launches the REAL @modelcontextprotocol/server-everything subprocess, and the
// server itself reports its environment via printEnv — proving the credential
// landed under the server's own declared variable. Every case = a brand-new
// server working with zero code changes.
//
//	go test -tags mcpintegration -run TestDynamicAuth_AnyServer_E2E ./internal/modules/mcp/ -v
func TestDynamicAuth_AnyServer_E2E(t *testing.T) {
	cases := []struct {
		id        string // bare server id — unknown to the static catalog
		envVar    string // the credential env var the registry declares for it
		shorthand string // the shorthand the USER writes (intentionally != envVar)
		secret    string
	}{
		{"dynauthalpha", "DEMO_ACCESS_TOKEN", "token", "PROOF-ALPHA-111"},
		{"dynauthbeta", "ACME_API_KEY", "api_key", "PROOF-BETA-222"},
		{"dynauthgamma", "SVC_API_TOKEN", "key", "PROOF-GAMMA-333"},
		{"dynauthdelta", "WIDGET_TOKEN", "token", "PROOF-DELTA-444"},
	}

	// One fake registry: for any queried id it returns the REAL everything npm
	// package declaring that case's credential env var. This registry entry is the
	// ONLY thing a new server needs to exist in — no digitorn code, ever.
	byID := map[string]string{}
	for _, c := range cases {
		byID[c.id] = c.envVar
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envVar := byID[r.URL.Query().Get("search")]
		if envVar == "" {
			envVar = "GENERIC_TOKEN"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": []any{map[string]any{"server": map[string]any{
			"name": "io.test/everything",
			"packages": []any{map[string]any{
				"registry_type":         "npm",
				"identifier":            "@modelcontextprotocol/server-everything",
				"environment_variables": []any{map[string]any{"name": envVar}},
			}},
		}}}})
	}))
	defer srv.Close()

	orig := registryURL
	registryURL = srv.URL
	defer func() { registryURL = orig }()

	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			ctx := module.WithModuleConfig(context.Background(), map[string]any{
				"servers": map[string]any{c.id: map[string]any{c.shorthand: c.secret}},
			})
			m := New()
			defer m.pool.shutdown(context.Background())

			specs := m.LiveTools(ctx) // registry resolve + real subprocess, from a bare id
			if len(specs) == 0 {
				t.Skip("everything server unreachable (npx/network) — skipping")
			}
			// The everything server exposes get-env, which returns its environment.
			tool := "mcp_" + c.id + "__get-env"
			if !hasTool(specs, tool) {
				var names []string
				for i := range specs {
					names = append(names, specs[i].Name)
				}
				t.Fatalf("get-env not materialized for %s (got %d tools): %s", c.id, len(specs), strings.Join(names, ", "))
			}

			res, err := m.Invoke(ctx, tool, []byte(`{}`))
			if err != nil {
				t.Fatalf("invoke printEnv: %v", err)
			}
			data, _ := res.Data.(map[string]any)
			out, _ := data["output"].(string)
			// The live server reports its own env: the credential MUST be present
			// under the registry-declared var name — that is the dynamic wiring.
			if !strings.Contains(out, c.envVar) || !strings.Contains(out, c.secret) {
				t.Fatalf("dynamic wiring FAILED: expected %s=%s in the live process env, got:\n%s",
					c.envVar, c.secret, out)
			}
			t.Logf("PROVEN: unknown server %q + YAML `%s: %s` → live process env carries %s=%s (no code)",
				c.id, c.shorthand, c.secret, c.envVar, c.secret)
		})
	}
}
