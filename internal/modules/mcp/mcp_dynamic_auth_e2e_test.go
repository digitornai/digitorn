//go:build mcpintegration

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/pkg/module"
)

func TestDynamicAuth_AnyServer_E2E(t *testing.T) {
	cases := []struct {
		id        string
		envVar    string
		shorthand string
		secret    string
	}{
		{"dynauthalpha", "DEMO_ACCESS_TOKEN", "token", "PROOF-ALPHA-111"},
		{"dynauthbeta", "ACME_API_KEY", "api_key", "PROOF-BETA-222"},
		{"dynauthgamma", "SVC_API_TOKEN", "key", "PROOF-GAMMA-333"},
		{"dynauthdelta", "WIDGET_TOKEN", "token", "PROOF-DELTA-444"},
	}

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

			specs := m.LiveTools(ctx)
			if len(specs) == 0 {
				t.Skip("everything server unreachable (npx/network) — skipping")
			}
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
			if !strings.Contains(out, c.envVar) || !strings.Contains(out, c.secret) {
				t.Fatalf("dynamic wiring FAILED: expected %s=%s in the live process env, got:\n%s",
					c.envVar, c.secret, out)
			}
			t.Logf("PROVEN: unknown server %q + YAML `%s: %s` → live process env carries %s=%s (no code)",
				c.id, c.shorthand, c.secret, c.envVar, c.secret)
		})
	}
}
