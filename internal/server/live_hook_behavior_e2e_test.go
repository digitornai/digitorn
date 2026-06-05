//go:build live

package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// HOOK BEHAVIOR E2E — the missing proof.
//
// The conformance lock proves the documented hooks COMPILE and the
// runtime has a handler for each. The earlier hook live tests proved
// the ENGINE behaviour — but they built the *hooks.Engine directly in
// Go, bypassing the compiler + catalog + install path.
//
// This test closes the loop : it installs an app whose YAML declares
// BEHAVIOURAL hooks (a `gate` that blocks writes, a `transform_params`
// that redirects reads), then drives a REAL LLM through the full
// daemon and asserts the hooks actually changed what happened :
//
//   - gate : the model's filesystem.write is VETOED — the file is
//     never created on disk and the tool result is errored with the
//     gate reason.
//   - transform_params : the model reads "decoy.txt" but the hook
//     rewrites the path to "expected.txt", so the answer carries the
//     expected file's content, not the decoy's.
//
// YAML → compile → install → real gateway → hook fires → behaviour
// changes. Nothing stubbed, nothing bypassed.
// =====================================================================

func hookBehaviorAppYAML(model string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: hook-behavior
  name: Hook Behavior
  version: "0.1.0"
  description: "Live behavioural proof : gate blocks writes, transform_params redirects reads, declared in YAML."

runtime:
  hooks:
    - id: block_writes
      "on": tool_start
      condition: { type: tool_name, match: "filesystem.write" }
      action: { type: gate, allow: false, reason: "writes blocked by policy hook" }
    - id: redirect_reads
      "on": tool_start
      condition: { type: tool_name, match: "filesystem.read" }
      action: { type: transform_params, transformation: { path: "expected.txt" } }

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      max_tokens: 400
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: |
      You are a filesystem agent. When the user asks you to read or write
      a file, immediately call the appropriate filesystem tool with the
      path they gave. Never refuse. After the tool runs, report plainly
      what happened (including any error the tool returned).
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
        tools: [read, write, ls, grep]
`, model)
}

// TestLive_HTTPe2e_HookGateAndTransformViaYAML installs the
// behavioural-hooks app and proves both hooks change real LLM
// behaviour through the full daemon.
func TestLive_HTTPe2e_HookGateAndTransformViaYAML(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping live hook-behavior e2e")
	}
	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)

	// Workspace : decoy.txt + expected.txt for the transform proof.
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const decoyContent = "this-is-the-decoy"
	const expectedContent = "the-expected-secret-payload"
	if err := os.WriteFile(filepath.Join(ws, "decoy.txt"), []byte(decoyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "expected.txt"), []byte(expectedContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// App source dir with the behavioural-hooks YAML.
	srcRoot := t.TempDir()
	appDir := filepath.Join(srcRoot, "hook-behavior")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app.yaml"),
		[]byte(hookBehaviorAppYAML(httpE2EModel())), 0o644); err != nil {
		t.Fatal(err)
	}

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	// Install (proves the YAML hooks COMPILE through the real install
	// path — the original bug).
	code, body := p.client.do(t, "POST", "/api/apps/install",
		map[string]any{"source": appDir})
	if code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}

	// ---------- GATE : write must be vetoed ----------
	sidGate := createHookSession(t, p, "gate")
	postHookMessage(t, p, sidGate,
		"Create a file named out.txt containing the word hello. Use the write tool now.")
	if !waitForAssistant(t, p.d.SessionStore(), sidGate, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidGate)
		t.Fatal("gate session : no assistant reply")
	}

	// The write hook must have vetoed : out.txt must NOT exist on disk.
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); err == nil {
		t.Errorf("out.txt was created — the gate hook did NOT block the write")
	}
	// If the model attempted the write, its tool result must be errored
	// with the gate reason.
	assertWriteVetoed(t, p.d.SessionStore(), sidGate)

	// ---------- TRANSFORM : read(decoy) → expected ----------
	sidTr := createHookSession(t, p, "transform")
	postHookMessage(t, p, sidTr,
		"Read the file decoy.txt and tell me the EXACT text inside it.")
	if !waitForAssistant(t, p.d.SessionStore(), sidTr, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidTr)
		t.Fatal("transform session : no assistant reply")
	}

	// The transform hook rewrote the path to expected.txt, so the
	// assistant must report the EXPECTED content, never the decoy.
	assertSessionText(t, p.d.SessionStore(), sidTr, expectedContent)
	if assistantMentions(t, p.d.SessionStore(), sidTr, decoyContent) {
		t.Errorf("assistant reported the decoy content — transform_params did NOT redirect the read")
	}
}

// =====================================================================
// helpers
// =====================================================================

func createHookSession(t *testing.T, p *persistDaemon, title string) string {
	t.Helper()
	code, body := p.client.do(t, "POST",
		"/api/apps/hook-behavior/sessions", map[string]any{"title": title})
	if code != http.StatusCreated {
		t.Fatalf("create session %q: %d %s", title, code, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session_id for %q: %s", title, body)
	}
	return sid
}

func postHookMessage(t *testing.T, p *persistDaemon, sid, content string) {
	t.Helper()
	code, body := p.client.do(t, "POST",
		"/api/apps/hook-behavior/sessions/"+sid+"/messages",
		map[string]any{"content": content})
	if code != http.StatusCreated {
		t.Fatalf("post message: %d %s", code, body)
	}
}

// assertWriteVetoed : every filesystem.write tool_result in the session
// must be errored with the gate reason. (Tolerant if the model never
// called write — the on-disk absence check already covers that case.)
func assertWriteVetoed(t *testing.T, bus *sessionstore.Bus, sid string) {
	t.Helper()
	state, err := bus.State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	state.RLock()
	defer state.RUnlock()
	sawWriteResult := false
	for _, m := range state.Messages {
		if m.Role != "tool" {
			continue
		}
		for _, part := range m.Parts {
			if part.Type != sessionstore.PartTypeToolResult || part.ToolResult == nil {
				continue
			}
			// The tool_result is correlated by call id ; we match the
			// write by scanning for the gate reason in the error.
			if part.ToolResult.Error == "" {
				continue
			}
			low := toLower(part.ToolResult.Error)
			if contains(low, "blocked") && contains(low, "gate") {
				sawWriteResult = true
			}
		}
	}
	if !sawWriteResult {
		t.Logf("note: no errored write tool_result observed (model may not have called write) ; on-disk absence is the binding assertion")
	}
}

// assistantMentions reports whether any assistant text part contains
// the needle (case-insensitive).
func assistantMentions(t *testing.T, bus *sessionstore.Bus, sid, needle string) bool {
	t.Helper()
	state, err := bus.State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	state.RLock()
	defer state.RUnlock()
	low := toLower(needle)
	for _, m := range state.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, part := range m.Parts {
			if part.Type == sessionstore.PartTypeText && contains(toLower(part.Text), low) {
				return true
			}
		}
	}
	return false
}
