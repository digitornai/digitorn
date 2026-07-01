//go:build live

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/config"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/server"
)

// =====================================================================
// E2E-2 — Live real disk persistence + cold rehydrate
//
// Two daemon lifecycles share the SAME on-disk state (sqlite DB +
// write-behind sharded session JSONL + installed app bundle) :
//
//   Daemon #1 : install an app, run a real LLM turn, flush, shut down.
//   Daemon #2 : cold start on the same paths. WITHOUT re-installing :
//     - the app must reappear (GORM Bootstrap reads the DB row +
//       loads the bundle from disk),
//     - the prior session must rehydrate from the JSONL on first
//       access (Bus.State cold-load), carrying the user message, the
//       assistant reply AND the filesystem.read tool_call.
//
// This is the durability proof the "100K concurrent sessions" target
// rests on : a daemon restart must lose nothing.
// =====================================================================

// persistDaemon bundles a running daemon + its HTTP client for one
// lifecycle of the cold-rehydrate test.
type persistDaemon struct {
	d      *server.Daemon
	cancel context.CancelFunc
	done   chan error
	client *httpClient
	port   int
}

// startPersistDaemon builds + starts a daemon on `port` wired to the
// shared disk paths, and waits until it is ready (HTTP up, LLM client
// + engine wired).
func startPersistDaemon(t *testing.T, port int, jwt, dbPath, sessionsRoot, appsRoot, binDir string, fsConfig []byte) *persistDaemon {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	cfg.Auth.Enabled = false
	cfg.Auth.DevMode = true
	cfg.Database.DSN = dbPath
	cfg.Sessions.Root = sessionsRoot
	cfg.Apps.Root = appsRoot
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
		Env: map[string]string{
			"DIGITORN_MODULE_FILESYSTEM_CONFIG": string(fsConfig),
		},
	}}

	d, err := server.Build(&cfg)
	if err != nil {
		t.Fatalf("server.Build(port=%d): %v", port, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	probe := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := probe.Get(base + "/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && d.LLM() != nil && d.Engine() != nil {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d.LLM() == nil || d.Engine() == nil {
		cancel()
		<-done
		t.Fatalf("daemon(port=%d) never became ready (llm=%v engine=%v)",
			port, d.LLM() != nil, d.Engine() != nil)
	}

	return &persistDaemon{
		d:      d,
		cancel: cancel,
		done:   done,
		port:   port,
		client: &httpClient{
			base: base, jwt: jwt, user: "test-user",
			htc: &http.Client{Timeout: 30 * time.Second},
		},
	}
}

// stop cancels the daemon context and waits for Start to return
// (Shutdown drains the flusher to disk).
func (p *persistDaemon) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Logf("daemon(port=%d) Start did not return after cancel within 15s", p.port)
	}
}

// TestLive_HTTPe2e_DiskPersistenceColdRehydrate proves a daemon
// restart loses nothing : the installed app and the full session
// transcript survive on disk and rehydrate on a fresh daemon.
func TestLive_HTTPe2e_DiskPersistenceColdRehydrate(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping live disk-persistence e2e")
	}
	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)

	// Shared on-disk state — survives BOTH daemon lifecycles because
	// t.TempDir() is scoped to the whole test function.
	sharedRoot := t.TempDir()
	dbPath := filepath.Join(sharedRoot, "shared.db")
	sessionsRoot := filepath.Join(sharedRoot, "sessions")
	appsRoot := filepath.Join(sharedRoot, "apps")

	// Workspace with a file the agent will read.
	ws := filepath.Join(sharedRoot, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const wantContent = "rendezvous-at-midnight"
	if err := os.WriteFile(filepath.Join(ws, "secret.txt"),
		[]byte(wantContent), 0o644); err != nil {
		t.Fatal(err)
	}
	fsConfig, _ := json.Marshal(map[string]any{
		"workspace":      ws,
		"max_file_bytes": 1048576,
	})

	appSrc := httpE2EWriteApp(t, httpE2EModel())

	basePort := httpE2EPort(t)
	port1, port2 := basePort, basePort+1

	var sid string

	// ============ DAEMON #1 : install + run a turn ============
	func() {
		p1 := startPersistDaemon(t, port1, jwt, dbPath, sessionsRoot, appsRoot, binDir, fsConfig)
		defer p1.stop(t)

		// Install.
		code, body := p1.client.do(t, "POST", "/api/apps/install",
			map[string]any{"source": appSrc})
		if code != http.StatusOK {
			t.Fatalf("install: %d %s", code, body)
		}

		// Create session.
		code, body = p1.client.do(t, "POST",
			"/api/apps/live-e2e-buddy/sessions",
			map[string]any{"title": "persist e2e"})
		if code != http.StatusCreated {
			t.Fatalf("create session: %d %s", code, body)
		}
		var created map[string]any
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatalf("decode session: %v", err)
		}
		sid, _ = created["session_id"].(string)
		if sid == "" {
			t.Fatalf("no session_id: %s", body)
		}
		t.Logf("session_id = %s", sid)

		// Post the user message → real LLM turn.
		msgPath := "/api/apps/live-e2e-buddy/sessions/" + sid + "/messages"
		code, body = p1.client.do(t, "POST", msgPath, map[string]any{
			"content": "Read secret.txt and tell me exactly what's inside.",
		})
		if code != http.StatusCreated {
			t.Fatalf("post message: %d %s", code, body)
		}

		if !waitForAssistant(t, p1.d.SessionStore(), sid, 90*time.Second) {
			dumpSessionEvents(t, p1.d.SessionStore(), sid)
			t.Fatal("daemon#1 : no assistant reply within 90s")
		}

		// Sanity on daemon #1 : the turn worked.
		assertSessionHasTurn(t, p1.d.SessionStore(), sid, wantContent, "daemon#1")

		// Force the write-behind flusher to drain to disk BEFORE we
		// shut down — belt-and-suspenders ; Shutdown also drains.
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		p1.d.SessionFlusher().Flush(fctx)
		fcancel()
	}()

	// At this point daemon #1 is fully shut down. The DB file, the
	// session JSONL and the app bundle are all on disk.

	// ============ DAEMON #2 : cold start, same disk ============
	p2 := startPersistDaemon(t, port2, jwt, dbPath, sessionsRoot, appsRoot, binDir, fsConfig)
	defer p2.stop(t)

	// (a) The app must have been bootstrapped from the DB — NOT
	//     re-installed. GET /api/apps lists it.
	code, body := p2.client.do(t, "GET", "/api/apps", nil)
	if code != http.StatusOK {
		t.Fatalf("list apps: %d %s", code, body)
	}
	if !containsAppID(t, body, "live-e2e-buddy") {
		t.Fatalf("app live-e2e-buddy not bootstrapped from DB on cold start : %s", body)
	}
	t.Log("app rehydrated from DB on cold start ✓")

	// (b) The session must cold-load from JSONL on first access.
	//     Hit the HTTP history endpoint (real client path) AND the
	//     bus projection.
	code, body = p2.client.do(t, "GET",
		"/api/apps/live-e2e-buddy/sessions/"+sid+"/history", nil)
	if code != http.StatusOK {
		t.Fatalf("cold history: %d %s", code, body)
	}
	low := toLower(string(body))
	if !contains(low, toLower(wantContent)) {
		t.Errorf("cold-loaded history missing file content %q : %s", wantContent, body)
	}

	// (c) Deep assertion on the rehydrated projection : user message,
	//     assistant reply with the content, and the filesystem.read
	//     tool_call must all be present after cold load.
	assertSessionHasTurn(t, p2.d.SessionStore(), sid, wantContent, "daemon#2(cold)")

	t.Log("session transcript rehydrated from disk on cold start ✓")
}

// =====================================================================
// helpers (local to the persistence test)
// =====================================================================

// assertSessionHasTurn checks the projected session carries the user
// message, an assistant text reply echoing wantContent, and a
// filesystem.read tool_call.
func assertSessionHasTurn(t *testing.T, bus *sessionstore.Bus, sid, wantContent, label string) {
	t.Helper()
	state, err := bus.State(sid)
	if err != nil {
		t.Fatalf("%s: State(%s): %v", label, sid, err)
	}
	state.RLock()
	defer state.RUnlock()

	var sawUser, sawAssistantContent, sawToolRead bool
	for _, m := range state.Messages {
		switch m.Role {
		case "user":
			for _, p := range m.Parts {
				if p.Type == sessionstore.PartTypeText &&
					contains(toLower(p.Text), "secret.txt") {
					sawUser = true
				}
			}
		case "assistant":
			for _, p := range m.Parts {
				if p.Type == sessionstore.PartTypeText &&
					contains(toLower(p.Text), toLower(wantContent)) {
					sawAssistantContent = true
				}
				if p.Type == sessionstore.PartTypeToolCall && p.ToolCall != nil {
					if p.ToolCall.Name == "filesystem.read" ||
						p.ToolCall.Name == "filesystem__read" {
						sawToolRead = true
					}
				}
			}
		}
	}
	if !sawUser {
		t.Errorf("%s: user message 'secret.txt' missing from projection", label)
	}
	if !sawToolRead {
		t.Errorf("%s: filesystem.read tool_call missing from projection", label)
	}
	if !sawAssistantContent {
		t.Errorf("%s: assistant reply doesn't echo %q", label, wantContent)
	}
}

func containsAppID(t *testing.T, body []byte, appID string) bool {
	t.Helper()
	// /api/apps returns either an array of apps or an object with an
	// "apps" key depending on the handler ; tolerate both by doing a
	// substring match on the marshalled body (app_id is unique enough).
	return contains(string(body), `"`+appID+`"`)
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
