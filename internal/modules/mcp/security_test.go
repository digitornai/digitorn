package mcp

import (
	"os"
	"strings"
	"testing"
)

func TestBuildSafeEnv(t *testing.T) {
	os.Setenv("MCP_TEST_HOST_SECRET", "leak")
	defer os.Unsetenv("MCP_TEST_HOST_SECRET")

	env := buildSafeEnv(map[string]string{"MY_VAR": "v1", "DATABASE_URL": "secret"})
	got := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}

	if got["MY_VAR"] != "v1" {
		t.Error("declared env var must pass through")
	}
	if _, ok := got["DATABASE_URL"]; ok {
		t.Error("blocked env var must be dropped even when explicitly declared")
	}
	if _, ok := got["MCP_TEST_HOST_SECRET"]; ok {
		t.Error("non-allow-listed host var must not be inherited")
	}
	hasPath := false
	for k := range got {
		if strings.EqualFold(k, "PATH") {
			hasPath = true
		}
	}
	if !hasPath {
		t.Error("PATH must be inherited")
	}
}
