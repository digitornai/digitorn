//go:build live

package server_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// waitForAssistantCount waits until the session has at least `want`
// assistant messages (one per completed turn), so a multi-turn driver
// waits for EACH turn to finish instead of racing on a stale reply.
func waitForAssistantCount(t *testing.T, bus *sessionstore.Bus, sid string, want int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := bus.State(sid)
		if err == nil && st != nil {
			n := 0
			snap := st.Snapshot()
			for _, m := range snap.Messages {
				if m.Role == "assistant" {
					n++
				}
			}
			if n >= want {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// =====================================================================
// CONTEXT COMPACTION LIVE E2E — the proof that compact_context is REAL.
//
// Installs an app whose YAML declares the documented auto-compaction
// pattern (a context_pressure-style hook firing compact_context), drives
// a REAL LLM through several turns, and asserts a durable
// context_compacted marker lands — i.e. the production compactor ran and
// shrank the model's view. Before the SessionCompactor was wired,
// compact_context was a silent no-op in prod ; this test is the binding
// proof that it now works end-to-end.
// =====================================================================

func compactionAppYAML(model string) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: compaction-app
  name: Compaction App
  version: "0.1.0"

runtime:
  context:
    max_tokens: 8000
    strategy: truncate
    keep_recent: 2
  hooks:
    - id: compact_on_growth
      "on": turn_end
      condition: { type: message_count, threshold: 3 }
      action: { type: compact_context, strategy: truncate, keep_last: 2 }

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.1
      max_tokens: 150
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "You are a terse assistant. Reply in one short sentence."

tools:
  capabilities:
    default_policy: auto
`, model)
}

// TestLive_HTTPe2e_ContextCompactionViaYAML proves the production
// compactor fires through a real LLM turn loop and records a durable
// context-compaction marker.
func TestLive_HTTPe2e_ContextCompactionViaYAML(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping live compaction e2e")
	}
	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()

	srcRoot := t.TempDir()
	writeHookApp(t, srcRoot, "compaction-app", compactionAppYAML(httpE2EModel()))

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	installHookApp(t, p, filepath.Join(srcRoot, "compaction-app"))

	sid := createSessionFor(t, p, "compaction-app", "compaction")

	// Drive several turns so message_count crosses the threshold and the
	// compact_context hook fires.
	prompts := []string{
		"My name is Paul. Remember it.",
		"What is 2 + 2?",
		"Name a primary colour.",
		"What city is the Eiffel Tower in?",
	}
	for i, msg := range prompts {
		postMessageFor(t, p, "compaction-app", sid, msg)
		// Wait for THIS turn's assistant reply (count must reach i+1),
		// not just any prior reply — otherwise turns race.
		if !waitForAssistantCount(t, p.d.SessionStore(), sid, i+1, 90*time.Second) {
			dumpSessionEvents(t, p.d.SessionStore(), sid)
			t.Fatalf("turn %d: assistant reply #%d never arrived", i+1, i+1)
		}
		// Give the (sync) compaction hook a beat to project after turn_end.
		time.Sleep(200 * time.Millisecond)
	}

	// A real compaction must have been recorded in the live session.
	st, err := p.d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	snap := st.Snapshot()
	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
		dumpSessionEvents(t, p.d.SessionStore(), sid)
		t.Fatalf("no context-compaction marker after %d real turns — compact_context did not fire", len(prompts))
	}
	t.Logf("live compaction OK: strategy=%s cutoff_seq=%d keep_recent=%d",
		snap.ContextCompaction.Strategy, snap.ContextCompaction.CutoffSeq, snap.ContextCompaction.KeepRecent)

	// The agent must still answer coherently AFTER compaction.
	postMessageFor(t, p, "compaction-app", sid, "What is the capital of France?")
	if !waitForAssistant(t, p.d.SessionStore(), sid, 90*time.Second) {
		t.Fatal("post-compaction turn: no assistant reply")
	}
	assertSessionText(t, p.d.SessionStore(), sid, "Paris")
}
