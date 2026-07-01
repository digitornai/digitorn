package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// captureLLM is the TURN LLM (not the summary one): returns a canned reply +
// usage so a turn completes, recording the last prompt it was asked to send.
type captureLLM struct {
	mu       sync.Mutex
	lastMsgs []llm.ChatMessage
}

func (c *captureLLM) Chat(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	c.mu.Lock()
	c.lastMsgs = append([]llm.ChatMessage(nil), req.Messages...)
	c.mu.Unlock()
	return &llm.ChatResponse{Content: "ok", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}}, nil
}

// ctx8App declares a context config whose small window + low threshold makes a
// modest seeded usage trip the per-round compaction guard, using strategy
// "summarize" so the CTX-8 non-blocking path is the one exercised.
func ctx8App() *fakeApps {
	return &fakeApps{app: &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "ctx8app", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "ctx8app", Name: "CTX8 E2E", Version: "1.0"},
			Agents: []schema.Agent{{
				ID: "main", Role: "assistant",
				// The guard reads the WINDOW from the agent's brain.context — a small
				// window so the seeded usage (900) crosses the threshold.
				Brain:        schema.Brain{Provider: "fake", Model: "fake-1", Context: &schema.ContextConfig{MaxTokens: 1000}},
				SystemPrompt: "be terse",
			}},
			Runtime: &schema.RuntimeBlock{
				ToolInjection: schema.ToolInjectionDirect,
				Context:       &schema.ContextConfig{MaxTokens: 1000, Strategy: "summarize", KeepRecent: 2, CompressionTrigger: 0.5},
			},
			Tools: &schema.ToolsBlock{Capabilities: &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}},
		},
	}}
}

func seedOverPressure(t *testing.T, bus *sessionstore.Bus, sid string) {
	t.Helper()
	seedWithApp(t, bus, sid, "ctx8app", 8)
	// Provider usage 700+200 = 900 → pressure 0.9 > 0.5 threshold.
	if _, err := bus.AppendDurable(context.Background(), sessionstore.Event{
		Type: sessionstore.EventTokenUsage, SessionID: sid, AppID: "ctx8app",
		Cost: &sessionstore.CostPayload{TokensIn: 700, TokensOut: 200},
	}); err != nil {
		t.Fatal(err)
	}
}

func newCtx8Engine(t *testing.T, bus *sessionstore.Bus, apps *fakeApps, summaryLLM chatLLM) *runtime.Engine {
	t.Helper()
	e, err := runtime.New(apps, bus, &captureLLM{}, discardLog())
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	comp := newContextCompactor(bus, apps, summaryLLM, discardLog())
	comp.nonBlocking = true
	e.Compactor = comp
	e.MicroCompactView = true
	return e
}

// TestCTX8_E2E_GuardSummarizesInline is the END-TO-END proof: a real turn runs
// through the real Engine.Run under context pressure; the per-round guard fires
// and, with no background-prepared summary ready, compacts via an INLINE
// summarize (fidelity preserved — never a lossy truncate). The summary LLM is
// reached on this turn's goroutine, which is correct and isolated per session.
func TestCTX8_E2E_GuardSummarizesInline(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-e2e-inline"
	apps := ctx8App()
	seedOverPressure(t, bus, sid)

	summary := &fakeLLM{reply: "E2E-INLINE-SUMMARY"}
	e := newCtx8Engine(t, bus, apps, summary)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "ctx8app", SessionID: sid, UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq == 0 {
		t.Fatalf("the guard did not compact under pressure (0.9 > 0.5): %+v", snap.ContextCompaction)
	}
	if snap.ContextCompaction.Strategy != "summarize" {
		t.Errorf("strategy = %q, want summarize (inline fidelity, never truncate)", snap.ContextCompaction.Strategy)
	}
	if summary.callCount() == 0 {
		t.Error("summary LLM was never called — inline summarize must run when no prepared summary is ready")
	}
	t.Logf("E2E PROVEN: real turn compacted inline (cutoff=%d, strategy=%s), facts preserved",
		snap.ContextCompaction.CutoffSeq, snap.ContextCompaction.Strategy)
}

// TestCTX8_E2E_AppliesPreparedSummary: with a background-prepared summary present,
// the real loop's guard applies it INSTANTLY (high fidelity) — still no LLM call.
func TestCTX8_E2E_AppliesPreparedSummary(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-e2e-prepared"
	apps := ctx8App()
	seedOverPressure(t, bus, sid)
	injectPrepared(t, bus, sid, "E2E-PREPARED-SUMMARY", 5)

	summary := &fakeLLM{panicOnCall: true}
	e := newCtx8Engine(t, bus, apps, summary)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "ctx8app", SessionID: sid, UserID: "u"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := summary.callCount(); n != 0 {
		t.Fatalf("summary LLM called %d times — applying a prepared summary must not call it", n)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil {
		t.Fatal("no compaction marker")
	}
	if snap.ContextCompaction.Strategy != "summarize" || snap.ContextCompaction.Summary != "E2E-PREPARED-SUMMARY" {
		t.Errorf("guard did not apply the prepared summary through the real loop: strategy=%s summary=%q",
			snap.ContextCompaction.Strategy, snap.ContextCompaction.Summary)
	}
}
