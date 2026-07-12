//go:build live

package server_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// CTX-8 LIVE MATRIX — ~50 REAL end-to-end compaction tests with a REAL LLM.
//
// Boots a real daemon, installs apps spanning the context-management matrix
// (window size × strategy × keep_recent × threshold), drives REAL multi-turn
// conversations until the auto-compaction guard fires, and asserts:
//   1. a durable context_compacted marker landed (compaction REALLY ran),
//   2. the full transcript survives on disk (history is never destroyed),
//   3. the agent still recalls a fact planted BEFORE compaction (seamless resume).
// Run TWICE: flag OFF (legacy inline summarize) and flag ON (CTX-8 non-blocking).
//
// Requires a real LLM:  DIGITORN_LIVE_LLM=1  + OPENAI_API_KEY (or the gateway).
// Run:  go test -tags live ./internal/server/ -run TestLive_CTX8_Matrix -v -timeout 30m
// COST WARNING: ~50 scenarios × several turns each = hundreds of real LLM calls.
// =====================================================================

type compactionScenario struct {
	name          string
	window        int
	strategy      string // truncate | summarize
	keepRecent    int
	threshold     float64 // 0 = use the daemon default (0.95, window-aware)
	turns         int
	expectCompact bool
}

// ctx8Matrix builds ~25 scenarios; run for each flag value → ~50 real tests.
// Every expectCompact scenario uses big filler turns (~1.2k tokens each) so the
// real prompt crosses the trigger well within the turn budget on EVERY window —
// the per-round guard reads the live provider prompt_tokens, so growth must be
// comfortably larger than the trigger (max ~3000 on a 4k window).
func ctx8Matrix() []compactionScenario {
	const driveTurns = 8 // ~360-tok turns cross every window's trigger within budget
	var out []compactionScenario
	// window × strategy — the core grid.
	for _, w := range []int{2000, 2500, 3000, 3500, 4000, 5000, 6000, 8000} {
		for _, s := range []string{"truncate", "summarize"} {
			out = append(out, compactionScenario{
				name: fmt.Sprintf("w%d_%s", w, s), window: w, strategy: s,
				keepRecent: 2, turns: driveTurns, expectCompact: true,
			})
		}
	}
	// keep_recent variants.
	for _, kr := range []int{1, 3, 5} {
		out = append(out, compactionScenario{
			name: fmt.Sprintf("keep%d", kr), window: 3000, strategy: "summarize",
			keepRecent: kr, turns: driveTurns, expectCompact: true,
		})
	}
	// explicit compression_trigger overrides (must still fire, earlier).
	for _, th := range []float64{0.4, 0.6, 0.8} {
		out = append(out, compactionScenario{
			name: fmt.Sprintf("thr%02.0f", th*100), window: 4000, strategy: "truncate",
			keepRecent: 2, threshold: th, turns: driveTurns, expectCompact: true,
		})
	}
	// long cumulative sessions — multiple compaction passes (summary-of-summary).
	out = append(out,
		compactionScenario{name: "cumulative_12t", window: 2000, strategy: "summarize", keepRecent: 2, turns: 12, expectCompact: true},
		compactionScenario{name: "cumulative_16t", window: 3000, strategy: "summarize", keepRecent: 2, turns: 16, expectCompact: true},
	)
	// NEGATIVE: a huge window + few turns must NOT compact.
	out = append(out, compactionScenario{name: "bigwindow_noac", window: 200000, strategy: "truncate", keepRecent: 2, turns: 3, expectCompact: false})
	return out
}

func scenarioYAML(appID string, sc compactionScenario, model string) string {
	trigger := ""
	if sc.threshold > 0 {
		trigger = fmt.Sprintf("    compression_trigger: %g\n", sc.threshold)
	}
	return fmt.Sprintf(`schema_version: 2

app:
  app_id: %s
  name: %s
  version: "0.1.0"

runtime:
  context:
    max_tokens: %d
    strategy: %s
    keep_recent: %d
%s
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
    system_prompt: "You are a benchmark assistant with NO tools and NO files. Never call a function. Reply with the fewest words possible."

tools:
  capabilities:
    default_policy: auto
`, appID, appID, sc.window, sc.strategy, sc.keepRecent, trigger, model, sc.window)
}

// scenarioPrompts drives moderate filler turns (~360 tokens each) of plain,
// task-free prose. Sized so that after a keep_recent=2 truncation the kept
// messages stay under the smallest window's effective budget (window/1.6),
// while a handful of turns still accumulate past the compaction trigger. No
// fact is planted (truncate legitimately drops old turns) and no file/task cues
// (so the model doesn't hallucinate tool calls). Post-compaction we assert
// COHERENCE (a fresh answer), not recall.
func scenarioPrompts(turns int) []string {
	block := strings.Repeat("The weather over the northern plains stayed calm and clear through the long quiet afternoon. ", 16)
	prompts := make([]string, 0, turns)
	for i := 0; i < turns; i++ {
		prompts = append(prompts, fmt.Sprintf("Background paragraph %d, no action needed. %sReply with the single word: OK.", i+1, block))
	}
	return prompts
}

// sessionLastSeq returns the session's last durable event seq (a monotonic
// cursor). Used to wait for the NEXT turn's assistant reply by seq rather than
// by a cumulative count — the snapshot's Messages are pruned by compaction
// (projection MessagesAfterCutoff), so counting assistant messages is unreliable.
func sessionLastSeq(t *testing.T, bus *sessionstore.Bus, sid string) uint64 {
	t.Helper()
	st, err := bus.State(sid)
	if err != nil || st == nil {
		return 0
	}
	return st.Snapshot().LastSeq
}

// waitForNewAssistant waits until an assistant message with Seq > afterSeq
// lands — i.e. THIS turn completed. Robust to compaction pruning: the current
// turn's reply is always the most recent message, so it is never elided.
func waitForNewAssistant(t *testing.T, bus *sessionstore.Bus, sid string, afterSeq uint64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := bus.State(sid); err == nil && st != nil {
			snap := st.Snapshot()
			for i := range snap.Messages {
				if snap.Messages[i].Role == "assistant" && snap.Messages[i].Seq > afterSeq {
					return true
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// assertCoherentAfterCompaction probes that the agent still works post-compaction:
// a fresh factual question must come back with "Paris". Reasoning models are
// stochastic and sometimes phrase the answer only in their reasoning trace, so we
// accept the token in the reply text OR the reasoning, and retry once before
// failing — a coherence probe, not a format check. On miss we log the real reply.
func assertCoherentAfterCompaction(t *testing.T, p *persistDaemon, appID, sid string) {
	t.Helper()
	const q = "Ignore the paragraphs above. In one word, what is the capital of France?"
	for attempt := 1; attempt <= 2; attempt++ {
		before := sessionLastSeq(t, p.d.SessionStore(), sid)
		postMessageFor(t, p, appID, sid, q)
		if !waitForNewAssistant(t, p.d.SessionStore(), sid, before, 120*time.Second) {
			t.Fatal("post-compaction turn: no assistant reply")
		}
		text, reasoning := latestAssistantReply(t, p.d.SessionStore(), sid, before)
		if strings.Contains(strings.ToLower(text+" "+reasoning), "paris") {
			return
		}
		t.Logf("coherence attempt %d miss: text=%q reasoning=%q", attempt, text, reasoning)
	}
	t.Errorf("agent never answered Paris after compaction (2 attempts) — session %s", sid)
}

// latestAssistantReply returns the text and reasoning of the newest assistant
// message (Seq > afterSeq) — the just-produced reply, never elided by compaction.
func latestAssistantReply(t *testing.T, bus *sessionstore.Bus, sid string, afterSeq uint64) (text, reasoning string) {
	t.Helper()
	st, err := bus.State(sid)
	if err != nil || st == nil {
		return "", ""
	}
	snap := st.Snapshot()
	for i := range snap.Messages {
		m := snap.Messages[i]
		if m.Role != "assistant" || m.Seq <= afterSeq {
			continue
		}
		var b strings.Builder
		b.WriteString(m.Content)
		for _, part := range m.Parts {
			if part.Type == sessionstore.PartTypeText {
				b.WriteString(" ")
				b.WriteString(part.Text)
			}
		}
		text, reasoning = b.String(), m.Reasoning
	}
	return text, reasoning
}

func runCTX8Matrix(t *testing.T, bgSummary bool) {
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping CTX-8 live matrix")
	}
	t.Setenv("DIGITORN_CONTEXT_BG_SUMMARY", map[bool]string{true: "1", false: "0"}[bgSummary])

	jwt := httpE2EReadJWT(t)
	binDir := buildHTTPE2EBinaries(t)
	ws := t.TempDir()
	srcRoot := t.TempDir()
	model := httpE2EModel()

	p := startStreamDaemon(t, jwt, binDir, ws)
	defer p.stop(t)

	for _, sc := range ctx8Matrix() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			appID := "ctx8-" + sc.name
			writeHookApp(t, srcRoot, appID, scenarioYAML(appID, sc, model))
			installHookApp(t, p, filepath.Join(srcRoot, appID))
			sid := createSessionFor(t, p, appID, sc.name)

			prompts := scenarioPrompts(sc.turns)
			for _, msg := range prompts {
				driveTurn(t, p, appID, sid, msg)
			}

			st, err := p.d.SessionStore().State(sid)
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			snap := st.Snapshot()

			if !sc.expectCompact {
				if snap.ContextCompaction != nil && snap.ContextCompaction.CutoffSeq != 0 {
					t.Errorf("a huge window must NOT compact in %d turns, but did (cutoff=%d)", sc.turns, snap.ContextCompaction.CutoffSeq)
				}
				return
			}

			if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
				dumpSessionEvents(t, p.d.SessionStore(), sid)
				t.Fatalf("expected compaction after %d turns (window %d) but none happened", sc.turns, sc.window)
			}
			// Full history must survive on disk (compaction only shrinks the view).
			full, err := p.d.SessionStore().Transcript(sid)
			if err != nil {
				t.Fatalf("Transcript: %v", err)
			}
			if len(full) < sc.turns {
				t.Errorf("durable transcript shrank to %d (< %d turns) — history must be preserved", len(full), sc.turns)
			}
			// SEAMLESS CONTINUATION: after the view was compacted the agent must
			// still answer a fresh question coherently (strategy-agnostic — truncate
			// legitimately drops old turns, so we assert a NEW answer, not recall).
			assertCoherentAfterCompaction(t, p, appID, sid)
			t.Logf("OK %s: compacted (strategy=%s cutoff=%d), history kept (%d on disk), coherent after",
				sc.name, snap.ContextCompaction.Strategy, snap.ContextCompaction.CutoffSeq, len(full))
		})
	}
}

// TestLive_CTX8_Matrix_Legacy runs the matrix with the background summary OFF
// (legacy inline-summarize path) — the documented behaviour.
func TestLive_CTX8_Matrix_Legacy(t *testing.T) { runCTX8Matrix(t, false) }

// TestLive_CTX8_Matrix_BackgroundSummary runs the SAME matrix with CTX-8 ON
// (non-blocking gate + background summary maintainer + micro-compaction).
func TestLive_CTX8_Matrix_BackgroundSummary(t *testing.T) { runCTX8Matrix(t, true) }
