//go:build live

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/config"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/server"

	// In-proc module registrations are not consumed in this test (we use
	// worker pools for filesystem), but importing them here keeps the
	// in-proc registry warm in case the worker pool ever falls back.
	_ "github.com/digitornai/digitorn/internal/modules/filesystem"
)

// =====================================================================
// E2E-1 — Live HTTP server → runtime → gateway
//
// The most faithful end-to-end path the daemon can produce :
//
//  - Real *server.Daemon built from config (server.Build + Start).
//  - Real HTTP listener on an ephemeral port, real chi router with the
//    full auth + session middleware chain.
//  - Real digitorn-worker subprocess hosting the filesystem module.
//  - Real digitorn-worker-llm subprocess routing to the digitorn LLM
//    gateway.
//  - Real app installed via POST /api/apps/install (the same path a
//    UI client would drive).
//  - Real POST /api/apps/{id}/sessions/{sid}/messages with Bearer
//    JWT — the request a real CLI/web client sends.
//  - Real runtime.hooks[] declared in YAML — wired through the
//    new production hookSource (E2E-0). FireCount > 0 proves the
//    JVM is alive in production.
// =====================================================================

// ------------- worker binary builds (cached across tests) ----------

var (
	httpE2EBinariesOnce sync.Once
	httpE2EBinDir       string
	httpE2EBinErr       error
)

func buildHTTPE2EBinaries(t *testing.T) string {
	t.Helper()
	httpE2EBinariesOnce.Do(func() {
		dir, err := os.MkdirTemp("", "live-http-e2e-bins-*")
		if err != nil {
			httpE2EBinErr = err
			return
		}
		for _, pkg := range []string{
			"github.com/digitornai/digitorn/cmd/digitorn-worker",
			"github.com/digitornai/digitorn/cmd/digitorn-worker-llm",
		} {
			name := filepath.Base(pkg)
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			out := filepath.Join(dir, name)
			cmd := exec.Command("go", "build", "-o", out, pkg)
			cmd.Stdout = io.Discard
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				httpE2EBinErr = fmt.Errorf("build %s: %w", pkg, err)
				return
			}
		}
		httpE2EBinDir = dir
	})
	if httpE2EBinErr != nil {
		t.Fatalf("build live e2e binaries: %v", httpE2EBinErr)
	}
	return httpE2EBinDir
}

// ------------- JWT discovery (gateway-mode bearer) -----------------

func httpE2EReadJWT(t *testing.T) string {
	t.Helper()
	if jwt := os.Getenv("DIGITORN_DEV_JWT"); jwt != "" {
		return jwt
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir: " + err.Error())
	}
	data, err := os.ReadFile(filepath.Join(home, ".digitorn", "credentials.json"))
	if err != nil {
		t.Skip("no DIGITORN_DEV_JWT and no ~/.digitorn/credentials.json")
	}
	const key = `"access_token"`
	idx := strings.Index(string(data), key)
	if idx == -1 {
		t.Skip("credentials.json has no access_token")
	}
	rest := string(data[idx+len(key):])
	q1 := strings.Index(rest, `"`)
	if q1 == -1 {
		t.Skip("malformed credentials.json")
	}
	rest = rest[q1+1:]
	q2 := strings.Index(rest, `"`)
	if q2 == -1 {
		t.Skip("malformed credentials.json (no closing quote)")
	}
	return rest[:q2]
}

func httpE2EGateway() string {
	if v := os.Getenv("DIGITORN_LLM_GATEWAY_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8002/v1"
}

func httpE2EModel() string {
	if v := os.Getenv("DIGITORN_LIVE_LLM_MODEL"); v != "" {
		return v
	}
	return "copilot-gpt-4o-mini"
}

// ------------- realistic app.yaml (modelled on opencode + file-organizer)

const appYAML = `schema_version: 2

app:
  app_id: live-e2e-buddy
  name: Live E2E Buddy
  version: "0.1.0"
  description: "Live E2E test app : LLM agent with filesystem tools and a runtime hook proving the production daemon wires runtime.hooks[]."
  author: "live-test@digitorn.local"
  category: "coding"

runtime:
  hooks:
    - id: count_tool_starts
      on: tool_start
      condition:
        type: always
      action:
        type: log
        message: "tool_start fired for {{tool.name}}"
        level: info

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.2
      max_tokens: 512
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: |
      You are a coding assistant with filesystem tools. When the user
      asks you to read a file, you MUST immediately call filesystem.read.
      Don't guess file contents — always read them. Be concise.
    modules:
      - filesystem

tools:
  modules:
    filesystem:
      config:
        workspace: "."
        max_file_bytes: 1048576
  capabilities:
    default_policy: auto
    grant:
      - module: filesystem
        tools: [read, ls, glob, grep]
`

// ------------- helpers for HTTP client work -------------------------

func httpE2EWriteApp(t *testing.T, model string) string {
	t.Helper()
	srcRoot := t.TempDir()
	appDir := filepath.Join(srcRoot, "live-e2e-buddy")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app source: %v", err)
	}
	body := fmt.Sprintf(appYAML, model)
	if err := os.WriteFile(filepath.Join(appDir, "app.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	return appDir
}

type httpClient struct {
	base string
	jwt  string
	user string
	htc  *http.Client
}

func (c *httpClient) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.Header.Set("X-User-ID", c.user)
	}
	if c.jwt != "" {
		req.Header.Set("Authorization", "Bearer "+c.jwt)
	}
	resp, err := c.htc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// httpE2EPort returns a TCP port unlikely to collide with other tests
// in this package, derived from PID and time.
func httpE2EPort(t *testing.T) int {
	return 19000 + (os.Getpid() % 900) + int(time.Now().UnixNano()%100)
}

// =====================================================================
// THE TEST
// =====================================================================

// TestLive_HTTPe2e_PostMessageDrivesRealLLMTurn fires a real POST
// /messages on a real HTTP listener of a freshly-built daemon, and
// asserts every observable layer behaved as documented :
//
//	(a) HTTP response 201 with seq + session_id.
//	(b) The runtime ran async : an EventAssistantMessage with a text
//	    reply landed in the session bus, AND filesystem.read tool_call
//	    / tool_result events were recorded in the same session.
//	(c) The assistant's final text mentions the file contents.
//	(d) The production hookSource wired in buildEngine fired the
//	    app's `count_tool_starts` runtime hook — FireCount > 0 proves
//	    the runtime.hooks[] block is no longer dormant.
func TestLive_HTTPe2e_PostMessageDrivesRealLLMTurn(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping live HTTP e2e")
	}
	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	t.Logf("worker binaries built in %s", binDir)

	// ---- Workspace with a file to read --------------------------
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const wantContent = "supercalifragilistic"
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"),
		[]byte(wantContent), 0o644); err != nil {
		t.Fatal(err)
	}
	fsConfig, _ := json.Marshal(map[string]any{
		"workspace":      ws,
		"max_file_bytes": 1048576,
	})

	// ---- App source on disk --------------------------------------
	appSrc := httpE2EWriteApp(t, httpE2EModel())

	// ---- Daemon config (defaults + LLM + filesystem worker pool)
	port := httpE2EPort(t)
	cfg := config.Defaults()
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	cfg.Auth.Enabled = false
	cfg.Auth.DevMode = true
	cfg.Database.DSN = filepath.Join(t.TempDir(), "e2e.db")
	cfg.Sessions.Root = filepath.Join(t.TempDir(), "sessions")
	cfg.Apps.Root = filepath.Join(t.TempDir(), "apps")
	cfg.Logging.Level = "warn"

	// LLM worker → gateway.
	cfg.Workers.LLM.Count = 1
	cfg.Workers.LLM.BinaryPath = filepath.Join(binDir, workerBinName("digitorn-worker-llm"))
	cfg.Workers.LLM.GatewayURL = httpE2EGateway()
	cfg.Workers.LLM.StartTimeout = 20 * time.Second

	// Filesystem via worker pool (proven path).
	cfg.Workers.Pools = []config.WorkerPool{{
		ID:           "fs-pool",
		Modules:      []string{"filesystem"},
		Count:        1,
		BinaryPath:   filepath.Join(binDir, workerBinName("digitorn-worker")),
		StartTimeout: 15 * time.Second,
		Env: map[string]string{
			"DIGITORN_MODULE_FILESYSTEM_CONFIG": string(fsConfig),
		},
	}}

	d, err := server.Build(&cfg)
	if err != nil {
		t.Fatalf("server.Build: %v", err)
	}
	// MountAPI is already called by Build() ; double-mounting panics chi.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- d.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-startErr:
		case <-time.After(10 * time.Second):
			t.Log("daemon Start did not return after cancel within 10s")
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &httpClient{
		base: base, jwt: jwt, user: "test-user",
		htc: &http.Client{Timeout: 30 * time.Second},
	}

	// ---- Wait for /ready (tolerant : the listener takes a few hundred
	//      ms to bind ; swallow early ECONNREFUSED so we don't fatal).
	waitDeadline := time.Now().Add(40 * time.Second)
	probe := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(waitDeadline) {
		resp, err := probe.Get(base + "/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && d.LLM() != nil && d.Engine() != nil {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d.LLM() == nil {
		t.Fatal("LLM worker never came up")
	}
	if d.Engine() == nil {
		t.Fatal("runtime engine never wired")
	}

	// ---- Install the app via real HTTP --------------------------
	code, body := client.do(t, "POST", "/api/apps/install",
		map[string]any{"source": appSrc})
	if code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}

	// ---- Create a session ---------------------------------------
	code, body = client.do(t, "POST",
		"/api/apps/live-e2e-buddy/sessions",
		map[string]any{"title": "live e2e"})
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %s", code, body)
	}
	var created map[string]any
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session_id in response: %s", body)
	}
	t.Logf("session_id = %s", sid)

	// ---- POST the user message — this kicks the engine ---------
	msgPath := "/api/apps/live-e2e-buddy/sessions/" + sid + "/messages"
	code, body = client.do(t, "POST", msgPath, map[string]any{
		"content": "Read hello.txt and tell me what's inside.",
	})
	if code != http.StatusCreated {
		t.Fatalf("post message: %d %s", code, body)
	}

	// ---- Poll the session bus for the assistant reply ----------
	if !waitForAssistant(t, d.SessionStore(), sid, 90*time.Second) {
		dumpSessionEvents(t, d.SessionStore(), sid)
		t.Fatal("no assistant message landed within 90s")
	}

	// ---- Assertions on the persisted session --------------------
	state, err := d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	state.RLock()
	var assistantText string
	var toolReadCalled bool
	for _, m := range state.Messages {
		for _, p := range m.Parts {
			if p.Type == sessionstore.PartTypeText && m.Role == "assistant" {
				assistantText += p.Text
			}
			if p.Type == sessionstore.PartTypeToolCall && p.ToolCall != nil {
				if p.ToolCall.Name == "filesystem.read" || p.ToolCall.Name == "filesystem__read" {
					toolReadCalled = true
				}
			}
		}
	}
	state.RUnlock()

	if !toolReadCalled {
		t.Errorf("filesystem.read was not part of the assistant tool_calls")
	}
	if !strings.Contains(strings.ToLower(assistantText),
		strings.ToLower(wantContent)) {
		t.Errorf("assistant reply doesn't echo file content %q : %q",
			wantContent, assistantText)
	}

	// ---- Hook engine — production wire-up assertion -------------
	// E2E-0 wired eng.Hooks. The YAML declares a noop hook on
	// tool_start. After the turn, FireCount("count_tool_starts")
	// must be > 0 — irrefutable proof the runtime.hooks[] block
	// is consumed by the production daemon.
	eng, ok := d.Engine().(*dgruntime.Engine)
	if !ok {
		t.Fatalf("daemon Engine is not *runtime.Engine: %T", d.Engine())
	}
	if eng.Hooks == nil {
		t.Fatal("eng.Hooks is nil — production wiring missing")
	}
	hookEng := eng.Hooks.ForApp("live-e2e-buddy")
	if hookEng == nil {
		t.Fatal("ForApp returned nil for installed app")
	}
	if got := hookEng.FireCount("count_tool_starts"); got == 0 {
		t.Errorf("count_tool_starts FireCount = 0 — hook never fired through the production daemon (JVM still dormant)")
	} else {
		t.Logf("count_tool_starts fired %d time(s)", got)
	}
}

// =====================================================================
// helpers
// =====================================================================

func workerBinName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// waitForAssistant polls the bus until an EventAssistantMessage with
// non-empty text content lands in the session.
func waitForAssistant(t *testing.T, bus *sessionstore.Bus, sid string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := bus.State(sid)
		if err == nil && state != nil {
			state.RLock()
			for _, m := range state.Messages {
				if m.Role != "assistant" {
					continue
				}
				for _, p := range m.Parts {
					if p.Type == sessionstore.PartTypeText && strings.TrimSpace(p.Text) != "" {
						state.RUnlock()
						return true
					}
				}
			}
			state.RUnlock()
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// dumpSessionEvents prints the session's projection so a failing
// test gives an actionable view of what the runtime produced.
func dumpSessionEvents(t *testing.T, bus *sessionstore.Bus, sid string) {
	t.Helper()
	state, err := bus.State(sid)
	if err != nil {
		t.Logf("dump: State(%s): %v", sid, err)
		return
	}
	state.RLock()
	defer state.RUnlock()
	t.Logf("--- session %s : %d messages ---", sid, len(state.Messages))
	for i, m := range state.Messages {
		t.Logf("  msg[%d] role=%s parts=%d", i, m.Role, len(m.Parts))
		for j, p := range m.Parts {
			switch p.Type {
			case sessionstore.PartTypeText:
				preview := p.Text
				if len(preview) > 120 {
					preview = preview[:120] + "..."
				}
				t.Logf("    part[%d] text=%q", j, preview)
			case sessionstore.PartTypeToolCall:
				if p.ToolCall != nil {
					t.Logf("    part[%d] tool_call name=%s args=%v",
						j, p.ToolCall.Name, p.ToolCall.Args)
				}
			case sessionstore.PartTypeToolResult:
				if p.ToolResult != nil {
					status := "ok"
					if p.ToolResult.Error != "" {
						status = "errored: " + p.ToolResult.Error
					}
					t.Logf("    part[%d] tool_result call_id=%s status=%s",
						j, p.ToolResult.ToolCallID, status)
				}
			}
		}
	}
}
