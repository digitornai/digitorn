package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Covers the decisions the project panel depends on but that no UI test can
// reach: what DNS a user must set for their own domain, which framework Vercel
// is told to build, which URL is handed to the user as "your site", and how an
// upstream failure is classified. Each of these has already produced a
// user-visible bug once (a protected team alias served as the public URL, a
// build failing on a missing framework hint), so they are pinned here.

func TestVercelDomainDNS(t *testing.T) {
	cases := []struct {
		domain   string
		wantType string
		wantName string
		wantVal  string
	}{
		{"boutique.fr", "A", "@", "76.76.21.21"},
		{"boutique.com", "A", "@", "76.76.21.21"},
		{"boutique.fr.", "A", "@", "76.76.21.21"},
		{"www.boutique.fr", "CNAME", "www", "cname.vercel-dns.com"},
		{"shop.boutique.fr", "CNAME", "shop", "cname.vercel-dns.com"},
		{"a.b.boutique.fr", "CNAME", "a.b", "cname.vercel-dns.com"},
	}
	for _, c := range cases {
		rec := vercelDomainDNS(c.domain)
		if len(rec) != 1 {
			t.Fatalf("%s: expected exactly one record, got %d", c.domain, len(rec))
		}
		if rec[0].Type != c.wantType || rec[0].Name != c.wantName || rec[0].Value != c.wantVal {
			t.Errorf("%s: got %s %s -> %s, want %s %s -> %s",
				c.domain, rec[0].Type, rec[0].Name, rec[0].Value, c.wantType, c.wantName, c.wantVal)
		}
	}
}

func TestVercelPublicURLIsCanonical(t *testing.T) {
	// Regression: the team-scoped alias returned by the API is behind
	// deployment protection and 404s for the user. The canonical
	// <project>.vercel.app is the only address safe to hand out.
	got := vercelPublicURL("digitorn-code-2e5005bc", []string{
		"digitorn-code-2e5005bc-team-abc.vercel.app",
		"digitorn-code-2e5005bc-git-main.vercel.app",
	})
	if got != "digitorn-code-2e5005bc.vercel.app" {
		t.Fatalf("got %q, want the canonical project host", got)
	}
}

func TestDetectFramework(t *testing.T) {
	cases := []struct {
		name string
		pkg  string
		want string
	}{
		{"vite", `{"devDependencies":{"vite":"^8"}}`, "vite"},
		{"vite in dependencies", `{"dependencies":{"vite":"^8"}}`, "vite"},
		{"next wins over vite", `{"dependencies":{"next":"15","vite":"^8"}}`, "nextjs"},
		{"cra", `{"dependencies":{"react-scripts":"5"}}`, "create-react-app"},
		{"sveltekit", `{"devDependencies":{"@sveltejs/kit":"2"}}`, "sveltekit"},
		{"unknown stack", `{"dependencies":{"lodash":"4"}}`, ""},
		{"malformed json", `{not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(c.pkg), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := detectFramework(dir); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	t.Run("no package.json", func(t *testing.T) {
		if got := detectFramework(t.TempDir()); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestCollectDeployFilesSkipsBuildArtefacts(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<html></html>")
	write("src/main.tsx", "export {}")
	write("node_modules/react/index.js", "module.exports={}")
	write(".git/config", "[core]")

	files, err := collectDeployFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.Path] = true
	}
	if !seen["index.html"] || !seen["src/main.tsx"] {
		t.Errorf("source files missing from upload set: %v", seen)
	}
	for p := range seen {
		if p == "node_modules/react/index.js" || p == ".git/config" {
			t.Errorf("%s must never be uploaded", p)
		}
	}
	for _, f := range files {
		if f.SHA == "" || f.Size == 0 {
			t.Errorf("%s: upload manifest needs a sha and a size, got %q / %d", f.Path, f.SHA, f.Size)
		}
	}
}

func TestVercelErrorClassification(t *testing.T) {
	e := vercelParseErr([]byte(`{"error":{"code":"forbidden","message":"You need to add a Login Connection to your GitHub account first"}}`))
	if e == nil {
		t.Fatal("expected a parsed error")
	}
	if !vercelIsGithubAppMissing(e) {
		t.Error("the GitHub login-connection failure must be recognised so the UI can offer the install link")
	}

	other := vercelParseErr([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
	if other != nil && vercelIsGithubAppMissing(other) {
		t.Error("an unrelated failure must not be reported as a missing GitHub app")
	}

	if got := vercelParseErr([]byte(`not json at all`)); got != nil && got.Error.Message == "" {
		t.Log("malformed upstream payload degrades to an empty error, which callers treat as generic")
	}
}

func TestSupabaseIsPaused(t *testing.T) {
	// A project that is not ACTIVE_HEALTHY has no API keys yet: the panel must
	// wake it instead of linking an empty connector.
	for _, s := range []string{"INACTIVE", "PAUSING", "RESTORING", "COMING_UP", "UNKNOWN", ""} {
		if s == "" {
			if supabaseIsPaused(s) {
				t.Errorf("empty status must not be treated as paused")
			}
			continue
		}
		if !supabaseIsPaused(s) {
			t.Errorf("%q must be treated as paused", s)
		}
	}
	if supabaseIsPaused("ACTIVE_HEALTHY") {
		t.Error("a healthy project must not be treated as paused")
	}
}

func TestSupabaseErrMessageSurfacesUpstreamText(t *testing.T) {
	msg := supabaseErrMessage([]byte(`{"message":"insufficient scope: projects_read"}`), nil, "fallback")
	if msg != "insufficient scope: projects_read" {
		t.Fatalf("got %q, want the upstream message so the user can act on it", msg)
	}
	if got := supabaseErrMessage([]byte(`garbage`), nil, "fallback"); got != "fallback" {
		t.Errorf("unparseable payload must degrade to the fallback, got %q", got)
	}
}

// Exercises the real HTTP path with a stubbed upstream: the request shape sent
// to Vercel is part of the contract and silently breaks when a path or header
// changes.
func TestVercelRequestShape(t *testing.T) {
	var gotPath, gotAuth, gotTeam, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		gotTeam = r.URL.Query().Get("teamId")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	old := vercelAPIBase
	vercelAPIBase = srv.URL
	defer func() { vercelAPIBase = old }()

	data, status, err := vercelRequest(context.Background(), http.MethodGet, "/v9/projects/demo/domains", "tok-123", "team-9", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotMethod != http.MethodGet || gotPath != "/v9/projects/demo/domains" {
		t.Errorf("upstream saw %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("authorization header = %q", gotAuth)
	}
	if gotTeam != "team-9" {
		t.Errorf("teamId query = %q, the call would hit the personal scope instead of the team", gotTeam)
	}
	if len(data) == 0 {
		t.Error("response body was not returned to the caller")
	}
}

func TestVercelRequestPropagatesUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"nope"}}`))
	}))
	defer srv.Close()

	old := vercelAPIBase
	vercelAPIBase = srv.URL
	defer func() { vercelAPIBase = old }()

	data, status, err := vercelRequest(context.Background(), http.MethodGet, "/v9/x", "t", "", nil)
	if err != nil {
		t.Fatalf("a 403 is an upstream answer, not a transport error: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 so the handler can map it", status)
	}
	if e := vercelParseErr(data); e == nil || e.Error.Message != "nope" {
		t.Errorf("upstream message lost: %+v", e)
	}
}
