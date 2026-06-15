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
// SUMMARIZE CARRIES CONTEXT — the hard proof the matrix did NOT give.
//
// The 50-scenario matrix proves compaction fires + history survives on disk +
// the agent stays coherent. It does NOT prove the SUMMARY actually carries
// forward conversation context, because its coherence probe is general
// knowledge ("Paris") that a summary-less truncate would also answer.
//
// These tests plant a distinctive codeword on turn 1, drive enough filler that
// turn 1 is dropped BEYOND keep_recent and a summarize-compaction folds it into
// the summary, then:
//   (1) assert the persisted summary is non-empty AND literally contains the
//       codeword (the summary preserved the key fact), and
//   (2) ask the agent to recall the codeword — which, since turn 1 is gone from
//       the model's view, can ONLY come from the injected summary (ApplyView
//       prepends it as a system message). That round-trip is the real proof.
//
// Run:  DIGITORN_LIVE_LLM=1 DIGITORN_LIVE_LLM_MODEL=mimo-v2.5 \
//         go test -tags live ./internal/server/ -run TestLive_CTX8_Summarize -v -timeout 20m
// =====================================================================

func summarizeRecallYAML(appID, model string, window int) string {
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
      max_tokens: 512
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
      context:
        max_tokens: %d
    system_prompt: "You are a benchmark assistant with NO tools and NO files. Carefully remember any codeword the user gives; you WILL be asked to recall it exactly later. Reply briefly."

tools:
  capabilities:
    default_policy: auto
`, appID, appID, window, model, window)
}

// driveTurn posts one message and waits for THIS turn's assistant reply (by seq).
// It is resilient to TRANSIENT provider faults (a gateway/upstream HTTP blip
// fails the turn with no reply): it re-drives the turn a few times before giving
// up, so a one-off infrastructure hiccup never fails an otherwise-correct test.
// A genuine logic failure still fails after the retries are exhausted.
func driveTurn(t *testing.T, p *persistDaemon, appID, sid, msg string) {
	t.Helper()
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		before := sessionLastSeq(t, p.d.SessionStore(), sid)
		postMessageFor(t, p, appID, sid, msg)
		if waitForNewAssistant(t, p.d.SessionStore(), sid, before, 120*time.Second) {
			return
		}
		if attempt < attempts {
			t.Logf("turn stalled (likely a transient provider fault) — re-driving %d/%d", attempt, attempts-1)
			time.Sleep(2 * time.Second)
		}
	}
	dumpSessionEvents(t, p.d.SessionStore(), sid)
	t.Fatalf("turn never completed after %d attempts: %.50s", attempts, msg)
}

// assertRecall asks for the codeword and accepts it in the reply text OR the
// reasoning trace (reasoning models often answer there), retrying once. Since
// the planting turn is dropped from the view, a hit proves the SUMMARY carried it.
func assertRecall(t *testing.T, p *persistDaemon, appID, sid, codeword string) {
	t.Helper()
	want := strings.ToLower(codeword)
	for attempt := 1; attempt <= 2; attempt++ {
		before := sessionLastSeq(t, p.d.SessionStore(), sid)
		postMessageFor(t, p, appID, sid, "What is the launch codeword I gave you at the very start? Reply with only the codeword.")
		if !waitForNewAssistant(t, p.d.SessionStore(), sid, before, 120*time.Second) {
			t.Fatal("recall turn: no assistant reply")
		}
		text, reasoning := latestAssistantReply(t, p.d.SessionStore(), sid, before)
		if strings.Contains(strings.ToLower(text+" "+reasoning), want) {
			t.Logf("recall OK (attempt %d): %q", attempt, strings.TrimSpace(text))
			return
		}
		t.Logf("recall attempt %d miss: text=%q reasoning=%q", attempt, text, reasoning)
	}
	t.Errorf("agent could NOT recall codeword %q after summarize-compaction — the summary did not carry the fact to the model", codeword)
}

func clipStrTest(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// runSummarizeRecall plants a codeword, drives filler until a summarize
// compaction folds it away, then asserts the summary kept it and the agent can
// still recall it. bgSummary toggles the legacy inline path (false) vs the
// CTX-8 non-blocking prepared-summary path (true).
func runSummarizeRecall(t *testing.T, bgSummary bool) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping summarize-recall live e2e")
	}
	t.Setenv("DIGITORN_CONTEXT_BG_SUMMARY", map[bool]string{true: "1", false: "0"}[bgSummary])

	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()
	srcRoot := t.TempDir()
	model := httpE2EModel()

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	appID := "ctx8-sumrecall"
	if bgSummary {
		appID = "ctx8-prepsum"
	}
	const codeword = "BLUEFALCON"

	writeHookApp(t, srcRoot, appID, summarizeRecallYAML(appID, model, 3000))
	installHookApp(t, p, filepath.Join(srcRoot, appID))
	sid := createSessionFor(t, p, appID, appID)

	// Turn 1: plant the codeword.
	driveTurn(t, p, appID, sid, "Remember the launch codeword exactly: "+codeword+"-7. Just reply OK.")

	// Filler turns push the codeword turn beyond keep_recent so summarize folds
	// it into the summary. The CTX-8 prepared path needs the background maintainer
	// to have produced a summary before a later compaction can apply it, so we
	// give it more turns and a brief settle between them.
	turns := 8
	if bgSummary {
		turns = 14
	}
	block := strings.Repeat("The harbor lights flickered softly over the calm evening water near the old pier. ", 16)
	for i := 0; i < turns; i++ {
		driveTurn(t, p, appID, sid, fmt.Sprintf("Filler note %d, no action needed. %sReply OK.", i+1, block))
		if bgSummary {
			time.Sleep(1200 * time.Millisecond) // let the background maintainer prepare
		}
	}

	st, err := p.d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	snap := st.Snapshot()

	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
		dumpSessionEvents(t, p.d.SessionStore(), sid)
		t.Fatal("no compaction happened — cannot test summary carry")
	}
	t.Logf("final compaction: strategy=%s cutoff=%d summary_len=%d",
		snap.ContextCompaction.Strategy, snap.ContextCompaction.CutoffSeq, len(snap.ContextCompaction.Summary))
	t.Logf("summary: %s", clipStrTest(snap.ContextCompaction.Summary, 500))

	// The summary message must exist and carry the planted fact. (In the CTX-8
	// non-blocking path the gate truncates until the maintainer's summary is
	// ready; a truncate-only run leaves an empty summary — which this asserts is
	// NOT the case, i.e. the prepared summary really did get applied.)
	if strings.TrimSpace(snap.ContextCompaction.Summary) == "" {
		t.Fatalf("summarize produced an EMPTY summary (strategy=%s) — context was dropped, not summarised", snap.ContextCompaction.Strategy)
	}
	if snap.ContextCompaction.Strategy != "summarize" {
		t.Errorf("expected the final compaction to be summarize, got %q (the summary path did not engage)", snap.ContextCompaction.Strategy)
	}
	if !strings.Contains(strings.ToLower(snap.ContextCompaction.Summary), strings.ToLower(codeword)) {
		t.Errorf("the summary does NOT contain the codeword %q — the summary dropped a key fact:\n%s", codeword, snap.ContextCompaction.Summary)
	}

	// End-to-end: the codeword turn is gone from the model's view, so a correct
	// recall can only come from the injected summary.
	assertRecall(t, p, appID, sid, codeword)
}

// TestLive_CTX8_SummarizeCarriesContext_Legacy proves the documented inline
// summarize path folds a fact into the summary and the model recalls it.
func TestLive_CTX8_SummarizeCarriesContext_Legacy(t *testing.T) { runSummarizeRecall(t, false) }

// TestLive_CTX8_SummarizeCarriesContext_Prepared proves the CTX-8 non-blocking
// path's background-prepared summary actually applies and carries context.
func TestLive_CTX8_SummarizeCarriesContext_Prepared(t *testing.T) { runSummarizeRecall(t, true) }
