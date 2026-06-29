//go:build live

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/config"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/server"
)

const flowAppYAML = `schema_version: 2

app:
  app_id: live-flow-e2e
  name: Live Flow E2E
  version: "0.1.0"
  description: "Flow app driven entirely through the real daemon HTTP API."
  author: "live-test@digitorn.local"
  category: "coding"

agents:
  - id: triage
    role: worker
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "Classify the request. Output JSON only."

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
        tools: [read, write]

flow:
  id: support
  entry: triage_node
  max_iterations: 20
  nodes:
    - id: triage_node
      type: agent
      agent: triage
      params:
        task: "Classify into refund or other. Respond ONLY with JSON {\"category\":\"<refund|other>\"}. Request: {{event.payload.message}}"
      routes:
        - { when: "category == 'refund'", to: refund_done }
        - { default: true, to: other_done }
    - id: refund_done
      type: terminal
      params:
        output: "FLOW_ROUTED_REFUND"
    - id: other_done
      type: terminal
      params:
        output: "FLOW_ROUTED_OTHER"
`

func writeFlowApp(t *testing.T, model string) string {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "live-flow-e2e")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := fmt.Sprintf(flowAppYAML, model)
	if err := os.WriteFile(filepath.Join(appDir, "app.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	return appDir
}

// TestLive_FlowHTTPe2e drives a FLOW app through the real production daemon:
// HTTP install -> create session -> POST message -> engine.Run -> runFlow ->
// agent classifies (real LLM) -> route -> tool writes a file (real worker).
// Proves the whole production path, not a test fixture.
func TestLive_FlowHTTPe2e(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set")
	}
	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)

	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	fsConfig, _ := json.Marshal(map[string]any{"workspace": ws, "max_file_bytes": 1048576})

	appSrc := writeFlowApp(t, httpE2EModel())

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
	cfg.Workers.LLM.Count = 1
	cfg.Workers.LLM.BinaryPath = filepath.Join(binDir, workerBinName("digitorn-worker-llm"))
	cfg.Workers.LLM.GatewayURL = httpE2EGateway()
	cfg.Workers.LLM.StartTimeout = 20 * time.Second
	cfg.Workers.Pools = []config.WorkerPool{{
		ID:           "fs-pool",
		Modules:      []string{"filesystem"},
		Count:        1,
		BinaryPath:   filepath.Join(binDir, workerBinName("digitorn-worker")),
		StartTimeout: 15 * time.Second,
		Env:          map[string]string{"DIGITORN_MODULE_FILESYSTEM_CONFIG": string(fsConfig)},
	}}

	d, err := server.Build(&cfg)
	if err != nil {
		t.Fatalf("server.Build: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- d.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-startErr:
		case <-time.After(10 * time.Second):
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &httpClient{base: base, jwt: jwt, user: "test-user", htc: &http.Client{Timeout: 30 * time.Second}}

	deadline := time.Now().Add(40 * time.Second)
	probe := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := probe.Get(base + "/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && d.Engine() != nil && d.LLM() != nil {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d.Engine() == nil || d.LLM() == nil {
		t.Fatal("daemon never became ready")
	}

	if code, body := client.do(t, "POST", "/api/apps/install", map[string]any{"source": appSrc}); code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}

	code, body := client.do(t, "POST", "/api/apps/live-flow-e2e/sessions", map[string]any{"title": "flow"})
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %s", code, body)
	}
	var created map[string]any
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session_id: %s", body)
	}

	msgPath := "/api/apps/live-flow-e2e/sessions/" + sid + "/messages"
	if code, body := client.do(t, "POST", msgPath, map[string]any{
		"content": "I want a refund for my broken order, please return my money",
	}); code != http.StatusCreated {
		t.Fatalf("post message: %d %s", code, body)
	}

	// The flow now persists a final assistant message — wait for it.
	if !waitForAssistant(t, d.SessionStore(), sid, 90*time.Second) {
		dumpSessionEvents(t, d.SessionStore(), sid)
		t.Fatal("flow produced no assistant message through the production daemon")
	}

	// Proof the real LLM classified the refund AND the flow routed to the
	// refund terminal — the terminal's output is the persisted assistant reply.
	state, err := d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	state.RLock()
	var assistantText string
	for _, m := range state.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == sessionstore.PartTypeText {
				assistantText += p.Text
			}
		}
	}
	state.RUnlock()

	if !strings.Contains(assistantText, "FLOW_ROUTED_REFUND") {
		dumpSessionEvents(t, d.SessionStore(), sid)
		t.Fatalf("flow did not route a refund through the daemon; assistant reply = %q", assistantText)
	}
	if strings.Contains(assistantText, "FLOW_ROUTED_OTHER") {
		t.Errorf("flow took the wrong branch; reply = %q", assistantText)
	}
	t.Logf("flow routed refund end-to-end through the production daemon: %q", assistantText)
}
