package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestCatalogListAndGet(t *testing.T) {
	all := CatalogList()
	if len(all) == 0 {
		t.Fatal("catalog is empty")
	}
	// Sorted by id, no dupes.
	for i := 1; i < len(all); i++ {
		if all[i-1].ServerID >= all[i].ServerID {
			t.Fatalf("catalog not strictly sorted: %q then %q", all[i-1].ServerID, all[i].ServerID)
		}
	}
	// github is a known catalog entry with an env-mapped credential.
	gh, ok := CatalogGet("github")
	if !ok {
		t.Fatal("github not in catalog")
	}
	if gh.Transport != "stdio" {
		t.Fatalf("github transport = %q, want stdio", gh.Transport)
	}
	if len(gh.RequiredFields) == 0 {
		t.Fatal("github has no required fields")
	}
	if _, ok := CatalogGet("does-not-exist-xyz"); ok {
		t.Fatal("phantom catalog hit")
	}
}

func TestCatalogOAuthFlag(t *testing.T) {
	// notion is OAuth-capable in the catalog.
	n, ok := CatalogGet("notion")
	if !ok {
		t.Skip("notion not in catalog")
	}
	if !n.HasOAuth {
		t.Fatal("notion should be flagged has_oauth")
	}
}

func TestSearchCatalogOnly(t *testing.T) {
	// A substring that only matches the catalog (offline; registry hit is a
	// best-effort network call that may add a result but never removes one).
	res := Search(context.Background(), "github")
	var found bool
	for _, r := range res {
		if r.ServerID == "github" && r.Source == "catalog" {
			found = true
		}
	}
	if !found {
		t.Fatalf("search 'github' did not return the github catalog entry: %+v", res)
	}
}

func TestRequirementsFromCatalog(t *testing.T) {
	req, ok := Requirements(context.Background(), "github")
	if !ok {
		t.Fatal("no requirements for github")
	}
	if req.Source != "catalog" {
		t.Fatalf("source = %q, want catalog", req.Source)
	}
	if len(req.Credentials) == 0 {
		t.Fatal("github requirements has no credentials")
	}
	if !strings.HasPrefix(req.YAMLExample, "github:") {
		t.Fatalf("yaml example does not start with the server id:\n%s", req.YAMLExample)
	}
	// The env var the server actually reads must be carried through.
	var hasEnv bool
	for _, c := range req.Credentials {
		if c.EnvVar != "" {
			hasEnv = true
		}
	}
	if !hasEnv {
		t.Fatal("no credential carries an env var")
	}
}

func TestYAMLExampleOAuth(t *testing.T) {
	out := yamlExample("notion", nil, true)
	if !strings.Contains(out, "auth: { type: oauth2 }") {
		t.Fatalf("oauth yaml missing auth block:\n%s", out)
	}
}

func TestRegistryShortID(t *testing.T) {
	cases := map[string]string{
		"io.github.foo/cool-server": "cool-server",
		"plain":                     "plain",
		"a/b/c":                     "c",
	}
	for in, want := range cases {
		if got := registryShortID(in); got != want {
			t.Fatalf("registryShortID(%q) = %q, want %q", in, got, want)
		}
	}
}
