//go:build live

package server_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// =====================================================================
// ULTRA-ADVANCED compaction tests — dimensions the matrix / recall / complex
// tests don't cover:
//
//  1. DECISION REVERSAL (evolving state): a fact is superseded (port 8080→9090,
//     Postgres→SQLite). After compaction the agent must report the CURRENT state,
//     not the stale one. This is the adversarial test for the fact-preservation
//     guard: a guard that blindly pins old tokens would force the obsolete value.
//
//  2. CONCURRENT ISOLATION: N sessions compact at the SAME time, each with its own
//     secret. Every session must recall ONLY its own — proving per-session
//     isolation (no cross-talk) AND that one agent's compaction never blocks the
//     others (they run in parallel against one daemon).
// =====================================================================

func reversalYAML(appID, model string, window int) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: %s
  name: %s
  version: "0.1.0"

runtime:
  context:
    max_tokens: %d
    strategy: summarize
    keep_recent: 2

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.0
      max_tokens: 600
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
      context:
        max_tokens: %d
    system_prompt: "You are an engineering assistant tracking a project's CURRENT state. When a decision changes, the LATEST value wins and the previous one is obsolete — never report a superseded value as current. You will be asked for the current state. Reply briefly and concretely."

tools:
  capabilities:
    default_policy: auto
`, appID, appID, window, model, window)
}

func runDecisionReversal(t *testing.T, bgSummary bool) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping decision-reversal e2e")
	}
	t.Setenv("DIGITORN_CONTEXT_BG_SUMMARY", map[bool]string{true: "1", false: "0"}[bgSummary])

	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()
	srcRoot := t.TempDir()
	model := httpE2EModel()

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	appID := "ctx8-reversal"
	if bgSummary {
		appID = "ctx8-reversal-bg"
	}
	writeHookApp(t, srcRoot, appID, reversalYAML(appID, model, 3000))
	installHookApp(t, p, filepath.Join(srcRoot, appID))
	sid := createSessionFor(t, p, appID, appID)

	// Initial decisions.
	driveTurn(t, p, appID, sid, "Initial architecture decisions: the database is Postgres; the service listens on port 8080; the cache TTL is 300 seconds. Reply: OK.")
	// A little progress, then SUPERSEDE two of them.
	driveTurn(t, p, appID, sid, "Build progress: the config loader is done. Reply: OK.")
	driveTurn(t, p, appID, sid, "DECISION CHANGES, effective immediately: we DROPPED Postgres and switched to SQLite at ./data/final.db. The service port changed from 8080 to 9090. The cache TTL stays 300 seconds. Postgres and port 8080 are now OBSOLETE — do not use them. Reply: OK.")

	// Filler pushes everything above out of the live view → only the summary remains.
	block := strings.Repeat("Routine telemetry line: nightly job ok, no anomalies, metrics nominal. ", 16)
	for i := 0; i < 8; i++ {
		driveTurn(t, p, appID, sid, fmt.Sprintf("Status note %d, no decision. %sReply: OK.", i+1, block))
	}

	st, err := p.d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	snap := st.Snapshot()
	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
		t.Fatal("no compaction happened — cannot test reversal")
	}
	t.Logf("compacted: strategy=%s cutoff=%d summary_len=%d", snap.ContextCompaction.Strategy, snap.ContextCompaction.CutoffSeq, len(snap.ContextCompaction.Summary))

	// CURRENT state must reflect the LATEST decisions, never the superseded ones.
	askExpect(t, p, appID, sid, "Which database are we using right now? Answer with just the database name.", "SQLite")
	askExpect(t, p, appID, sid, "What is the CURRENT service port number?", "9090")
	askExpect(t, p, appID, sid, "What is the cache TTL in seconds?", "300")
	// And the obsolete decision must be recognised as dead.
	askExpect(t, p, appID, sid, "Is Postgres still part of the current plan? Answer yes or no, then one short clause why.", "no")
}

// TestLive_CTX8_DecisionReversal_Legacy — inline summarize must track superseded facts.
func TestLive_CTX8_DecisionReversal_Legacy(t *testing.T) { runDecisionReversal(t, false) }

// TestLive_CTX8_DecisionReversal_Prepared — same, on the CTX-8 non-blocking path.
func TestLive_CTX8_DecisionReversal_Prepared(t *testing.T) { runDecisionReversal(t, true) }

// =====================================================================

// TestLive_CTX8_ConcurrentIsolation drives N sessions concurrently against ONE
// daemon, each planting a distinct codeword and compacting it out of its view.
// Every session must recall ONLY its own codeword — never another session's —
// proving per-session isolation under concurrent compaction.
func TestLive_CTX8_ConcurrentIsolation(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping concurrent-isolation e2e")
	}

	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()
	srcRoot := t.TempDir()
	model := httpE2EModel()

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	const appID = "ctx8-concurrent"
	writeHookApp(t, srcRoot, appID, summarizeRecallYAML(appID, model, 3000))
	installHookApp(t, p, filepath.Join(srcRoot, appID))

	codewords := []string{"ALPHA-11", "BETA-22", "GAMMA-33", "DELTA-44"}

	// Inner non-parallel run keeps the daemon alive until every parallel child
	// finishes (defer p.stop must not fire before the subtests execute).
	t.Run("sessions", func(t *testing.T) {
		for i, cw := range codewords {
			i, cw := i, cw
			t.Run(cw, func(t *testing.T) {
				t.Parallel()
				start := time.Now()
				sid := createSessionFor(t, p, appID, cw)
				driveTurn(t, p, appID, sid, "My session codeword is "+cw+". Remember it exactly; reply: OK.")

				block := strings.Repeat("Ambient note: harbor lights over calm water near the old pier. ", 16)
				for k := 0; k < 7; k++ {
					driveTurn(t, p, appID, sid, fmt.Sprintf("Filler %d for session %d. %sReply: OK.", k+1, i+1, block))
				}

				st, err := p.d.SessionStore().State(sid)
				if err != nil {
					t.Fatalf("State: %v", err)
				}
				if cc := st.Snapshot().ContextCompaction; cc == nil || cc.CutoffSeq == 0 {
					t.Fatal("session did not compact")
				}

				// Must recall its OWN codeword and NONE of the others (no cross-talk).
				before := sessionLastSeq(t, p.d.SessionStore(), sid)
				postMessageFor(t, p, appID, sid, "What is my session codeword? Reply with only the codeword.")
				if !waitForNewAssistant(t, p.d.SessionStore(), sid, before, 120*time.Second) {
					t.Fatal("no reply to recall")
				}
				text, reasoning := latestAssistantReply(t, p.d.SessionStore(), sid, before)
				ans := strings.ToLower(text + " " + reasoning)
				if !strings.Contains(ans, strings.ToLower(cw)) {
					t.Errorf("session %s could not recall its own codeword; got %q", cw, strings.TrimSpace(text))
				}
				for _, other := range codewords {
					if other == cw {
						continue
					}
					if strings.Contains(ans, strings.ToLower(other)) {
						t.Errorf("CROSS-CONTAMINATION: session %s leaked another session's codeword %q; got %q", cw, other, strings.TrimSpace(text))
					}
				}
				t.Logf("session %s isolated OK in %s", cw, time.Since(start).Round(time.Second))
			})
		}
	})
}

// =====================================================================

func toolReadYAML(appID, model string, window int) string {
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: %s
  name: %s
  version: "0.1.0"

runtime:
  context:
    max_tokens: %d
    strategy: summarize
    keep_recent: 2

agents:
  - id: assistant
    role: assistant
    brain:
      provider: openai
      model: %q
      temperature: 0.0
      max_tokens: 600
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
      context:
        max_tokens: %d
    system_prompt: "You are an assistant with filesystem tools. When asked to read a file, you MUST call filesystem.read — never guess its contents. Remember any value you read; you will be asked to recall it after a long gap."
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
        tools: [read]
`, appID, appID, window, model, window)
}

// TestLive_CTX8_ToolResultSurvivesCompaction proves a value the agent obtained
// from a TOOL CALL (filesystem.read) survives compaction: the tool call + its
// result are folded into the summary (Phase 3 renderTranscript), so after the
// call is dropped from the live view the agent still recalls what it read.
func TestLive_CTX8_ToolResultSurvivesCompaction(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping tool-result-survives-compaction e2e")
	}
	t.Setenv("DIGITORN_CONTEXT_BG_SUMMARY", "0")

	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	srcRoot := t.TempDir()
	model := httpE2EModel()

	ws := t.TempDir()
	const secret = "VAULT-CODE-7731"
	if err := os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("The vault access code is "+secret+". Do not lose it."), 0o644); err != nil {
		t.Fatal(err)
	}

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	const appID = "ctx8-toolread"
	writeHookApp(t, srcRoot, appID, toolReadYAML(appID, model, 3000))
	installHookApp(t, p, filepath.Join(srcRoot, appID))
	sid := createSessionFor(t, p, appID, appID)

	// The agent reads the file via a real tool call and notes the code. The path
	// must be the bare relative name (the workspace root holds the file).
	driveTurn(t, p, appID, sid, "Call filesystem.read with the path exactly: secret.txt (a bare relative name — no leading ./ , no slash, no directory). Then tell me you have noted the vault access code. Reply briefly.")

	// Push the tool call + its result out of the live view.
	block := strings.Repeat("Routine note: nightly batch ok, metrics nominal, nothing to action. ", 16)
	for i := 0; i < 8; i++ {
		driveTurn(t, p, appID, sid, fmt.Sprintf("Status %d, no action. %sReply: OK.", i+1, block))
	}

	st, err := p.d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	snap := st.Snapshot()
	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
		t.Fatal("no compaction happened — cannot test tool-result survival")
	}
	t.Logf("compacted: strategy=%s cutoff=%d summary_len=%d", snap.ContextCompaction.Strategy, snap.ContextCompaction.CutoffSeq, len(snap.ContextCompaction.Summary))

	// The value came ONLY from a tool result now compacted away — recall proves the
	// summary captured the tool outcome, not just chat text.
	askExpect(t, p, appID, sid, "What is the vault access code you read from secret.txt earlier? Reply with only the code.", secret)
}
