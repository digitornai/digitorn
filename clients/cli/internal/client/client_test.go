package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/client"
)

// ---- helpers -------------------------------------------------------

type routeKey struct{ Method, Path string }

// recordingMux is a tiny test mux that lets a test pre-program
// canned responses per (method, path) and asserts the inbound request.
type recordingMux struct {
	t        *testing.T
	handlers map[routeKey]http.HandlerFunc
	calls    []*http.Request
}

func newMux(t *testing.T) *recordingMux {
	return &recordingMux{
		t:        t,
		handlers: map[routeKey]http.HandlerFunc{},
	}
}

func (m *recordingMux) Handle(method, path string, h http.HandlerFunc) {
	m.handlers[routeKey{method, path}] = h
}

func (m *recordingMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.calls = append(m.calls, r)
	if h, ok := m.handlers[routeKey{r.Method, r.URL.Path}]; ok {
		h(w, r)
		return
	}
	http.Error(w, "no handler for "+r.Method+" "+r.URL.Path, http.StatusNotFound)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func newClientAgainst(t *testing.T, mux http.Handler) (*client.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := client.New(client.Options{
		BaseURL:     srv.URL,
		BearerToken: "test-jwt",
		UserID:      "user-A",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

// ---- New / Options ------------------------------------------------

func TestNew_RequiresBaseURL(t *testing.T) {
	_, err := client.New(client.Options{})
	if err == nil || !strings.Contains(err.Error(), "BaseURL") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_RejectsMalformedURL(t *testing.T) {
	_, err := client.New(client.Options{BaseURL: "not-a-url"})
	if err == nil {
		t.Fatal("expected error on missing scheme")
	}
}

// ---- Ping ----------------------------------------------------------

func TestPing_OK(t *testing.T) {
	mux := newMux(t)
	mux.Handle("GET", "/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]string{"status": "ok"})
	})
	c, _ := newClientAgainst(t, mux)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestPing_5xx(t *testing.T) {
	mux := newMux(t)
	mux.Handle("GET", "/health", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "dead", http.StatusServiceUnavailable)
	})
	c, _ := newClientAgainst(t, mux)
	if err := c.Ping(context.Background()); err == nil {
		t.Error("expected error on 503")
	}
}

// ---- Auth headers --------------------------------------------------

func TestHeaders_BearerAndUserID(t *testing.T) {
	mux := newMux(t)
	mux.Handle("GET", "/api/apps", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-jwt" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-User-ID") != "user-A" {
			t.Errorf("X-User-ID = %q", r.Header.Get("X-User-ID"))
		}
		writeJSON(t, w, 200, map[string]any{"apps": []any{}, "count": 0})
	})
	c, _ := newClientAgainst(t, mux)
	_, _ = c.ListApps(context.Background(), false)
}

// ---- Apps ----------------------------------------------------------

func TestListApps_DecodesPayload(t *testing.T) {
	mux := newMux(t)
	mux.Handle("GET", "/api/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{
			"apps": []map[string]any{
				{"app_id": "chat-simple", "name": "Chat Simple", "version": "0.1.0", "enabled": true, "byok": false},
				{"app_id": "rag-demo", "name": "RAG", "version": "1.2.0", "enabled": false, "byok": true},
			},
			"count": 2,
		})
	})
	c, _ := newClientAgainst(t, mux)
	apps, err := c.ListApps(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2", len(apps))
	}
	if apps[0].AppID != "chat-simple" || apps[1].BYOK != true {
		t.Errorf("apps decoded wrong : %+v", apps)
	}
}

func TestListApps_IncludeDisabledFlag(t *testing.T) {
	mux := newMux(t)
	var gotQuery string
	mux.Handle("GET", "/api/apps", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(t, w, 200, map[string]any{"apps": []any{}, "count": 0})
	})
	c, _ := newClientAgainst(t, mux)
	_, _ = c.ListApps(context.Background(), true)
	if gotQuery != "include_disabled=true" {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestGetApp_404DecodesAPIError(t *testing.T) {
	mux := newMux(t)
	mux.Handle("GET", "/api/apps/ghost", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 404, map[string]string{"error": "app_not_found", "message": "no such app"})
	})
	c, _ := newClientAgainst(t, mux)
	_, err := c.GetApp(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != 404 || apiErr.Code != "app_not_found" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestInstallApp_PassesSourceInBody(t *testing.T) {
	mux := newMux(t)
	var gotBody map[string]string
	mux.Handle("POST", "/api/apps/install", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(t, w, 200, map[string]any{
			"app_id": "chat-simple", "name": "Chat", "version": "0.1.0",
			"source": gotBody["source"], "install_dir": "/tmp/apps/chat-simple",
			"enabled": true, "byok": false,
		})
	})
	c, _ := newClientAgainst(t, mux)
	resp, err := c.InstallApp(context.Background(), "/path/to/chat")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["source"] != "/path/to/chat" {
		t.Errorf("source body = %q", gotBody["source"])
	}
	if resp.AppID != "chat-simple" {
		t.Errorf("appID = %q", resp.AppID)
	}
}

func TestSetBYOK_PutsEnabledField(t *testing.T) {
	mux := newMux(t)
	var got map[string]bool
	mux.Handle("PUT", "/api/apps/chat-simple/byok", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(t, w, 200, map[string]any{"app_id": "chat-simple", "byok": got["enabled"]})
	})
	c, _ := newClientAgainst(t, mux)
	if err := c.SetBYOK(context.Background(), "chat-simple", true); err != nil {
		t.Fatal(err)
	}
	if got["enabled"] != true {
		t.Errorf("body = %+v", got)
	}
}

// ---- Sessions ------------------------------------------------------

func TestCreateSession_PostsAndDecodes(t *testing.T) {
	mux := newMux(t)
	mux.Handle("POST", "/api/apps/chat-simple/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 201, map[string]any{
			"session_id": "sess-1", "app_id": "chat-simple", "seq": 1,
			"title": "test", "started_at": "2026-01-01T00:00:00Z",
		})
	})
	c, _ := newClientAgainst(t, mux)
	resp, err := c.CreateSession(context.Background(), "chat-simple", client.CreateSessionRequest{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SessionID != "sess-1" || resp.Seq != 1 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestHistory_AppendsQueryParams(t *testing.T) {
	mux := newMux(t)
	var gotQ string
	mux.Handle("GET", "/api/apps/chat-simple/sessions/sess-1/history", func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.RawQuery
		writeJSON(t, w, 200, map[string]any{
			"messages":      []any{},
			"pending_queue": []any{},
		})
	})
	c, _ := newClientAgainst(t, mux)
	_, err := c.History(context.Background(), "chat-simple", "sess-1", 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQ, "since=42") || !strings.Contains(gotQ, "limit=100") {
		t.Errorf("query = %q", gotQ)
	}
}

// ---- Messages ------------------------------------------------------

func TestPostMessage_HappyPath(t *testing.T) {
	mux := newMux(t)
	mux.Handle("POST", "/api/apps/chat-simple/sessions/sess-1/messages", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["content"] != "hi" || body["role"] != "user" {
			t.Errorf("body = %+v", body)
		}
		writeJSON(t, w, 201, map[string]any{
			"session_id": "sess-1", "seq": 2, "role": "user", "ts": "2026-01-01T00:00:00Z",
		})
	})
	c, _ := newClientAgainst(t, mux)
	resp, err := c.PostMessage(context.Background(), "chat-simple", "sess-1", "hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Seq != 2 {
		t.Errorf("seq = %d", resp.Seq)
	}
}

// ---- Discovery -----------------------------------------------------

func TestDiscover_EnvOverridesDefault(t *testing.T) {
	t.Setenv(client.EnvDaemonURL, "http://special:9999")
	if got := client.Discover(); got != "http://special:9999" {
		t.Errorf("Discover = %q", got)
	}
}

func TestDiscover_FallsBackToDefault(t *testing.T) {
	t.Setenv(client.EnvDaemonURL, "")
	if got := client.Discover(); got != client.DefaultDaemonURL {
		t.Errorf("Discover = %q, want %q", got, client.DefaultDaemonURL)
	}
}

func TestDiscoverAndPing_UnreachableReturnsTypedError(t *testing.T) {
	t.Setenv(client.EnvDaemonURL, "http://127.0.0.1:1") // port 1 = nobody home
	_, err := client.DiscoverAndPing(context.Background(), 200_000_000)
	if err == nil {
		t.Fatal("expected unreachable error")
	}
	if !client.IsUnreachable(err) {
		t.Errorf("err = %v, IsUnreachable=false", err)
	}
}

// ---- Auth file loading --------------------------------------------

func TestLoadCredentials_EnvOverridesFile(t *testing.T) {
	t.Setenv(client.CredentialsEnv, "env-jwt-token")
	creds, err := client.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || creds.AccessToken != "env-jwt-token" {
		t.Errorf("creds = %+v", creds)
	}
}

func TestLoadCredentials_FromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // Windows
	t.Setenv(client.CredentialsEnv, "")
	_ = os.Unsetenv(client.CredentialsEnv)

	credsDir := filepath.Join(dir, ".digitorn")
	if err := os.MkdirAll(credsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"access_token":"file-token","email":"u@x.com","user_id":"u1"}`
	if err := os.WriteFile(filepath.Join(credsDir, "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := client.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || creds.AccessToken != "file-token" || creds.Email != "u@x.com" {
		t.Errorf("creds = %+v", creds)
	}
}

func TestLoadCredentials_MissingFileIsNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	_ = os.Unsetenv(client.CredentialsEnv)
	creds, err := client.LoadCredentials()
	if err != nil {
		t.Fatalf("unauthenticated should be OK : %v", err)
	}
	if creds != nil {
		t.Errorf("creds = %+v, want nil", creds)
	}
}
