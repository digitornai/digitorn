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

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// HK-8 LIVE — real-LLM proof for the data-driven hook path.
//
// The deterministic 50-app guarantee
// (internal/runtime/hooks/fifty_apps_test.go) proves every documented
// condition/action/event compiles + fires through the real engine. THIS
// test closes the remaining loop : it drives a REAL LLM through the FULL
// daemon (install → gateway → turn → hook) and proves two things the
// stub layer cannot :
//
//   1. inject_message on turn_start fires on EVERY real turn and the
//      injected message is durably persisted as an observable user
//      message — no dependency on the model choosing to call a tool.
//
//   2. content_contains (a HK-7 data-driven condition fed from live turn
//      state) fires ONLY when the user's prompt actually contains the
//      keyword. A control session WITHOUT the keyword must NOT get the
//      injection — proving the condition truly gates in production.
//
// YAML → compile → install → real gateway → hook fires → message
// injected. Nothing stubbed.
// =====================================================================

func injectAlwaysAppYAML(model string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: hook-inject-always
  name: Hook Inject Always
  version: "0.1.0"

runtime:
  hooks:
    - id: inject_always
      "on": turn_start
      condition: { type: always }
      action: { type: inject_message, content: "DIGITORN-INJECT-ALWAYS", role: user }

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      max_tokens: 200
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "You are a terse assistant. Answer in one short sentence."

tools:
  capabilities:
    default_policy: auto
`, model)
}

func injectMultiAppYAML(model string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: hook-inject-multi
  name: Hook Inject Multi
  version: "0.1.0"

runtime:
  hooks:
    - id: inject_first
      "on": turn_start
      priority: 10
      condition: { type: always }
      action: { type: inject_message, content: "DIGITORN-INJECT-ONE", role: user }
    - id: inject_second
      "on": turn_start
      priority: 20
      condition: { type: always }
      action: { type: inject_message, content: "DIGITORN-INJECT-TWO", role: user }

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      max_tokens: 200
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "You are a terse assistant. Answer in one short sentence."

tools:
  capabilities:
    default_policy: auto
`, model)
}

func injectSessionStartAppYAML(model string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: hook-session-start
  name: Hook Session Start
  version: "0.1.0"

runtime:
  hooks:
    - id: on_session_start
      "on": session_start
      condition: { type: always }
      action: { type: inject_message, content: "DIGITORN-SESSION-START", role: user }

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      max_tokens: 200
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "You are a terse assistant. Answer in one short sentence."

tools:
  capabilities:
    default_policy: auto
`, model)
}

func injectKeywordAppYAML(model string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: hook-inject-keyword
  name: Hook Inject Keyword
  version: "0.1.0"

runtime:
  hooks:
    - id: inject_on_keyword
      "on": turn_start
      condition: { type: content_contains, keyword: "PINEAPPLE" }
      action: { type: inject_message, content: "DIGITORN-INJECT-KEYWORD", role: user }

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      max_tokens: 200
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "You are a terse assistant. Answer in one short sentence."

tools:
  capabilities:
    default_policy: auto
`, model)
}

// TestLive_HTTPe2e_InjectMessageAndContentContainsViaYAML installs the
// two inject apps and proves inject_message + content_contains behave as
// documented through a real LLM and the full daemon.
func TestLive_HTTPe2e_InjectMessageAndContentContainsViaYAML(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping live inject/content_contains e2e")
	}
	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()

	srcRoot := t.TempDir()
	writeHookApp(t, srcRoot, "hook-inject-always", injectAlwaysAppYAML(httpE2EModel()))
	writeHookApp(t, srcRoot, "hook-inject-multi", injectMultiAppYAML(httpE2EModel()))
	writeHookApp(t, srcRoot, "hook-session-start", injectSessionStartAppYAML(httpE2EModel()))
	writeHookApp(t, srcRoot, "hook-inject-keyword", injectKeywordAppYAML(httpE2EModel()))

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	installHookApp(t, p, filepath.Join(srcRoot, "hook-inject-always"))
	installHookApp(t, p, filepath.Join(srcRoot, "hook-inject-multi"))
	installHookApp(t, p, filepath.Join(srcRoot, "hook-session-start"))
	installHookApp(t, p, filepath.Join(srcRoot, "hook-inject-keyword"))

	const markAlways = "DIGITORN-INJECT-ALWAYS"
	const markKeyword = "DIGITORN-INJECT-KEYWORD"

	// ---------- inject_message on turn_start (always) ----------
	sidA := createSessionFor(t, p, "hook-inject-always", "always")
	postMessageFor(t, p, "hook-inject-always", sidA, "Say hi.")
	if !waitForAssistant(t, p.d.SessionStore(), sidA, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidA)
		t.Fatal("inject-always : no assistant reply")
	}
	if !userMessageContains(t, p.d.SessionStore(), sidA, markAlways) {
		dumpSessionEvents(t, p.d.SessionStore(), sidA)
		t.Errorf("inject_message(always) did NOT persist the injected user message %q", markAlways)
	}

	// ---------- TWO inject hooks on the same event → BOTH land ----------
	sidM := createSessionFor(t, p, "hook-inject-multi", "multi")
	postMessageFor(t, p, "hook-inject-multi", sidM, "Say hi.")
	if !waitForAssistant(t, p.d.SessionStore(), sidM, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidM)
		t.Fatal("inject-multi : no assistant reply")
	}
	if !userMessageContains(t, p.d.SessionStore(), sidM, "DIGITORN-INJECT-ONE") {
		dumpSessionEvents(t, p.d.SessionStore(), sidM)
		t.Errorf("first of two inject_message hooks was dropped (multi-inject regression)")
	}
	if !userMessageContains(t, p.d.SessionStore(), sidM, "DIGITORN-INJECT-TWO") {
		dumpSessionEvents(t, p.d.SessionStore(), sidM)
		t.Errorf("second of two inject_message hooks was dropped (multi-inject regression)")
	}

	// ---------- session_start : fires on the first turn ----------
	sidSS := createSessionFor(t, p, "hook-session-start", "session-start")
	postMessageFor(t, p, "hook-session-start", sidSS, "Say hi.")
	if !waitForAssistant(t, p.d.SessionStore(), sidSS, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidSS)
		t.Fatal("session-start : no assistant reply")
	}
	if !userMessageContains(t, p.d.SessionStore(), sidSS, "DIGITORN-SESSION-START") {
		dumpSessionEvents(t, p.d.SessionStore(), sidSS)
		t.Errorf("session_start hook did NOT fire on the first turn (newly-wired event dead in prod)")
	}

	// ---------- content_contains : keyword PRESENT → fires ----------
	sidK := createSessionFor(t, p, "hook-inject-keyword", "keyword-present")
	postMessageFor(t, p, "hook-inject-keyword", sidK,
		"I really love PINEAPPLE on pizza, what do you think?")
	if !waitForAssistant(t, p.d.SessionStore(), sidK, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidK)
		t.Fatal("inject-keyword(present) : no assistant reply")
	}
	if !userMessageContains(t, p.d.SessionStore(), sidK, markKeyword) {
		dumpSessionEvents(t, p.d.SessionStore(), sidK)
		t.Errorf("content_contains hook did NOT inject when the keyword was present")
	}

	// ---------- content_contains : keyword ABSENT → must NOT fire ----------
	sidC := createSessionFor(t, p, "hook-inject-keyword", "keyword-absent")
	postMessageFor(t, p, "hook-inject-keyword", sidC,
		"Tell me a fun fact about the ocean.")
	if !waitForAssistant(t, p.d.SessionStore(), sidC, 90*time.Second) {
		dumpSessionEvents(t, p.d.SessionStore(), sidC)
		t.Fatal("inject-keyword(absent) : no assistant reply")
	}
	if userMessageContains(t, p.d.SessionStore(), sidC, markKeyword) {
		dumpSessionEvents(t, p.d.SessionStore(), sidC)
		t.Errorf("content_contains hook injected even though the keyword was ABSENT — condition not gating live")
	}
}

// =====================================================================
// helpers
// =====================================================================

func writeHookApp(t *testing.T, srcRoot, appID, yaml string) {
	t.Helper()
	dir := filepath.Join(srcRoot, appID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installHookApp(t *testing.T, p *persistDaemon, appDir string) {
	t.Helper()
	code, body := p.client.do(t, "POST", "/api/apps/install", map[string]any{"source": appDir})
	if code != http.StatusOK {
		t.Fatalf("install %s: %d %s", appDir, code, body)
	}
}

func createSessionFor(t *testing.T, p *persistDaemon, appID, title string) string {
	t.Helper()
	code, body := p.client.do(t, "POST",
		"/api/apps/"+appID+"/sessions", map[string]any{"title": title})
	if code != http.StatusCreated {
		t.Fatalf("create session %q/%q: %d %s", appID, title, code, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session_id for %q/%q: %s", appID, title, body)
	}
	return sid
}

func postMessageFor(t *testing.T, p *persistDaemon, appID, sid, content string) {
	t.Helper()
	code, body := p.client.do(t, "POST",
		"/api/apps/"+appID+"/sessions/"+sid+"/messages",
		map[string]any{"content": content})
	if code != http.StatusCreated {
		t.Fatalf("post message %q: %d %s", appID, code, body)
	}
}

// userMessageContains reports whether any USER-role message in the
// session carries the needle (case-insensitive). inject_message persists
// the injected text as a user-role message, so this is the binding
// observation that the hook fired and its effect was applied.
func userMessageContains(t *testing.T, bus *sessionstore.Bus, sid, needle string) bool {
	t.Helper()
	state, err := bus.State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	state.RLock()
	defer state.RUnlock()
	low := toLower(needle)
	for _, m := range state.Messages {
		if m.Role != "user" {
			continue
		}
		for _, part := range m.Parts {
			if part.Type == sessionstore.PartTypeText && contains(toLower(part.Text), low) {
				return true
			}
		}
		if contains(toLower(m.Content), low) {
			return true
		}
	}
	return false
}
