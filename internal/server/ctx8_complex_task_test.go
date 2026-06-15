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
// COMPLEX TASK CONTINUATION — the real guarantee: after compaction the agent
// continues EXACTLY where it left off on a multi-fact engineering task.
//
// Unlike the single-codeword recall test, this plants a whole project state —
// codename, an explicit accept/reject decision, a hard constraint, two finished
// components, a precise numeric budget, and the immediate next step + its output
// format — then compacts ALL of it out of the live view behind filler. After
// compaction it interrogates each distinct fact AND asks the agent to state what
// it does next under which constraint, with NOTHING repeated. EVERY fact must
// survive (via the summary's KEY FACTS) or the test fails — a strict guarantee.
// =====================================================================

func complexTaskYAML(appID, model string, window int) string {
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
    system_prompt: "You are a senior engineering assistant on a long-running project. Remember ALL project decisions, constraints, progress, exact values, and the next step precisely — you will be asked to continue the work and recall specifics later. Reply briefly and concretely."

tools:
  capabilities:
    default_policy: auto
`, appID, appID, window, model, window)
}

// askExpect posts a question and requires EVERY token in want to appear in the
// fresh answer (text or reasoning), retrying once. AND semantics: a single
// missing fact fails. Since the planting turns are compacted out of the view,
// a hit can only come from the summary the agent now relies on.
func askExpect(t *testing.T, p *persistDaemon, appID, sid, question string, want ...string) {
	t.Helper()
	for attempt := 1; attempt <= 2; attempt++ {
		before := sessionLastSeq(t, p.d.SessionStore(), sid)
		postMessageFor(t, p, appID, sid, question)
		if !waitForNewAssistant(t, p.d.SessionStore(), sid, before, 120*time.Second) {
			t.Fatalf("no reply to: %s", question)
		}
		text, reasoning := latestAssistantReply(t, p.d.SessionStore(), sid, before)
		hay := strings.ToLower(text + " " + reasoning)
		var missing []string
		for _, w := range want {
			if !strings.Contains(hay, strings.ToLower(w)) {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			t.Logf("OK  %-62s -> %q", question, strings.TrimSpace(clipStrTest(text, 120)))
			return
		}
		t.Logf("attempt %d MISS %v  q=%q  text=%q", attempt, missing, question, strings.TrimSpace(clipStrTest(text, 160)))
	}
	t.Errorf("after compaction the agent FAILED to recover %v for: %q", want, question)
}

func runComplexTaskContinuation(t *testing.T, bgSummary bool) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping complex-task continuation e2e")
	}
	t.Setenv("DIGITORN_CONTEXT_BG_SUMMARY", map[bool]string{true: "1", false: "0"}[bgSummary])

	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()
	srcRoot := t.TempDir()
	model := httpE2EModel()

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	appID := "ctx8-complextask"
	if bgSummary {
		appID = "ctx8-complextask-bg"
	}
	writeHookApp(t, srcRoot, appID, complexTaskYAML(appID, model, 3000))
	installHookApp(t, p, filepath.Join(srcRoot, appID))
	sid := createSessionFor(t, p, appID, appID)

	// --- Establish the full project state (the work to be continued) ---
	driveTurn(t, p, appID, sid, "Project kickoff. Codename: ORCHID-9. Firm architecture decisions: storage is a SINGLE SQLite file at ./data/orchid.db — we explicitly REJECTED Postgres for this. Hard runtime rule: never block the UI thread; ALL I/O must be async via the EventLoop. The upstream API rate limit is exactly 47 requests per minute — never exceed it. Acknowledge with: OK.")
	driveTurn(t, p, appID, sid, "Progress so far: the parser component is fully DONE and tested, and the validator component is fully DONE and tested. Reply: OK.")
	driveTurn(t, p, appID, sid, "The immediate next step is to build the exporter component, which must write its output in Parquet format. Do not start it yet. Reply: OK.")

	// --- Filler to push all of the above out of the live view (compaction) ---
	block := strings.Repeat("Background log line: the nightly batch completed with no anomalies and metrics were within nominal range. ", 14)
	for i := 0; i < 8; i++ {
		driveTurn(t, p, appID, sid, fmt.Sprintf("Routine status note %d, no decision needed. %sReply: OK.", i+1, block))
	}

	st, err := p.d.SessionStore().State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	snap := st.Snapshot()
	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
		dumpSessionEvents(t, p.d.SessionStore(), sid)
		t.Fatal("no compaction happened — cannot test continuation")
	}
	t.Logf("compacted: strategy=%s cutoff=%d summary_len=%d", snap.ContextCompaction.Strategy, snap.ContextCompaction.CutoffSeq, len(snap.ContextCompaction.Summary))

	// --- THE guarantee, asked FIRST (most realistic "continue where you left off":
	// the agent resumes straight after compaction, before any interrogation can
	// perturb the state): the next action AND the hard constraint, unprompted. ---
	askExpect(t, p, appID, sid,
		"Continue the project. Without me repeating anything, state precisely the next component you must build and its required output format, plus the hard runtime rule you must respect.",
		"export", "Parquet", "EventLoop")

	// --- Then verify EVERY individual fact of the task state survived too ---
	askExpect(t, p, appID, sid, "What is the project codename? Answer briefly.", "ORCHID-9")
	askExpect(t, p, appID, sid, "What exact storage file path did we commit to?", "orchid.db")
	askExpect(t, p, appID, sid, "Which database engine did we explicitly reject?", "Postgres")
	askExpect(t, p, appID, sid, "What is the exact upstream rate limit, in requests per minute?", "47")
	askExpect(t, p, appID, sid, "Name the two components that are already finished.", "parser", "validator")
}

// TestLive_CTX8_ComplexTaskContinuation_Legacy — inline summarize path (flag OFF).
func TestLive_CTX8_ComplexTaskContinuation_Legacy(t *testing.T) { runComplexTaskContinuation(t, false) }

// TestLive_CTX8_ComplexTaskContinuation_Prepared — CTX-8 non-blocking path (flag ON).
func TestLive_CTX8_ComplexTaskContinuation_Prepared(t *testing.T) { runComplexTaskContinuation(t, true) }
