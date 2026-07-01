package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// fakeLLM is a chatLLM that records calls and can panic/sleep/fail on demand —
// the instrument that proves the turn loop never reaches the summary LLM.
type fakeLLM struct {
	calls       int32
	reply       string
	err         error
	sleep       time.Duration
	panicOnCall bool
}

func (f *fakeLLM) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.panicOnCall {
		panic("CTX-8 VIOLATION: the summary LLM was called on the turn loop")
	}
	if f.sleep > 0 {
		select {
		case <-time.After(f.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &llm.ChatResponse{Content: f.reply}, nil
}

func (f *fakeLLM) callCount() int32 { return atomic.LoadInt32(&f.calls) }

// fakeApps satisfies appmgr.Manager via embedding but only implements Get — the
// only method the compaction/summary path touches.
type fakeApps struct {
	appmgr.Manager
	app *appmgr.RuntimeApp
}

func (f *fakeApps) Get(context.Context, string) (*appmgr.RuntimeApp, error) { return f.app, nil }

func appsWithBrain() *fakeApps {
	return &fakeApps{app: &appmgr.RuntimeApp{
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{Brain: schema.Brain{Provider: "fake", Model: "fake-1"}}},
		},
	}}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// seedWithApp seeds messages carrying an AppID so resolveContextConfig can reach
// the app's brain (the LLM summarize path needs it).
func seedWithApp(t *testing.T, bus *sessionstore.Bus, sid, appID string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 1; i <= n; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		if _, err := bus.AppendDurable(ctx, sessionstore.Event{
			Type:      sessionstore.EventUserMessage,
			SessionID: sid,
			AppID:     appID,
			Message:   &sessionstore.MessagePayload{Role: role, Content: fmt.Sprintf("message number %d body text", i)},
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func injectPrepared(t *testing.T, bus *sessionstore.Bus, sid, summary string, coversSeq uint64) {
	t.Helper()
	if _, err := bus.AppendDurable(context.Background(), sessionstore.Event{
		Type:       sessionstore.EventContextSummaryPrepared,
		SessionID:  sid,
		CtxSummary: &sessionstore.ContextSummaryPayload{Summary: summary, CoversSeq: coversSeq},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCTX8_GateSummarizesInlineWhenNoPrepared: fidelity over latency. With no
// background-prepared summary ready, the gate summarises INLINE (calls the LLM
// once) rather than dropping facts via truncate. The call runs on THIS agent's
// turn goroutine only (a snapshot copy, no shared lock), never the daemon — so a
// few seconds on one agent is the accepted cost of a complete context.
func TestCTX8_GateSummarizesInlineWhenNoPrepared(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-inline"
	seedWithApp(t, bus, sid, "app1", 12)

	llm := &fakeLLM{reply: "INLINE-SUMMARY"}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())
	c.nonBlocking = true

	if err := c.CompactSession(context.Background(), sid, "summarize", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if n := llm.callCount(); n != 1 {
		t.Fatalf("summary LLM called %d times — want exactly 1 (inline summarize when no prepared summary)", n)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil {
		t.Fatal("no compaction marker — gate did not compact")
	}
	if snap.ContextCompaction.Strategy != "summarize" {
		t.Errorf("strategy = %q, want summarize (inline, facts preserved — never a lossy truncate)", snap.ContextCompaction.Strategy)
	}
}

// TestCTX8_AppliesPreparedSummaryInstantly: with a background-prepared summary
// ready, the gate applies it INSTANTLY (high fidelity) without any LLM call, and
// the consumed candidate is cleared.
func TestCTX8_AppliesPreparedSummaryInstantly(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-prep"
	seedWithApp(t, bus, sid, "app1", 12)
	injectPrepared(t, bus, sid, "PREPARED-HIGH-FIDELITY", 8)

	llm := &fakeLLM{panicOnCall: true}
	c := newContextCompactor(bus, nil, llm, discardLog())
	c.nonBlocking = true

	if err := c.CompactSession(context.Background(), sid, "summarize", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if n := llm.callCount(); n != 0 {
		t.Fatalf("LLM called %d times — applying a prepared summary must not call it", n)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil {
		t.Fatal("no compaction marker")
	}
	if snap.ContextCompaction.Strategy != "summarize" {
		t.Errorf("strategy = %q, want summarize (prepared applied)", snap.ContextCompaction.Strategy)
	}
	if snap.ContextCompaction.CutoffSeq != 8 {
		t.Errorf("cutoff = %d, want 8 (prepared CoversSeq)", snap.ContextCompaction.CutoffSeq)
	}
	if snap.ContextCompaction.Summary != "PREPARED-HIGH-FIDELITY" {
		t.Errorf("summary = %q, want the prepared high-fidelity one", snap.ContextCompaction.Summary)
	}
	if snap.PreparedSummary != nil {
		t.Errorf("prepared summary not cleared after consume: %+v", snap.PreparedSummary)
	}
}

// TestCTX8_FlagOffCallsLLMLegacy: with the flag OFF the path is exactly legacy —
// summarize calls the LLM inline. Proves the flag truly gates the new behaviour.
func TestCTX8_FlagOffCallsLLMLegacy(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-legacy"
	seedWithApp(t, bus, sid, "app1", 12)

	llm := &fakeLLM{reply: "LEGACY-SUMMARY"}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())
	c.nonBlocking = false // flag OFF

	if err := c.CompactSession(context.Background(), sid, "summarize", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if llm.callCount() == 0 {
		t.Fatal("legacy mode (flag off) must call the summary LLM inline")
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil || snap.ContextCompaction.Strategy != "summarize" {
		t.Errorf("legacy summarize did not record a summarize marker: %+v", snap.ContextCompaction)
	}
}

// TestCTX8_PromotesKeyFactsToWorkingMemory: a summarize compaction auto-promotes
// the summary's KEY FACTS into the lossless, never-compacted working-memory
// channel (snap.Facts), so they survive verbatim across every future compaction.
func TestCTX8_PromotesKeyFactsToWorkingMemory(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-promote"
	seedWithApp(t, bus, sid, "app1", 12)

	llm := &fakeLLM{reply: "KEY FACTS:\n- Codename: ORCHID-9\n- Rate limit: 47 rpm\n\nMISSION: build it."}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())

	if err := c.CompactSession(context.Background(), sid, "summarize", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	facts := strings.Join(mustSnap(t, bus, sid).Facts, " | ")
	if !strings.Contains(facts, "ORCHID-9") || !strings.Contains(facts, "47") {
		t.Errorf("KEY FACTS were not promoted to working memory (snap.Facts): %q", facts)
	}
}

// TestCTX8_MaintainerPreparesRealSummary: the background maintainer calls the LLM
// and persists a high-fidelity prepared candidate.
func TestCTX8_MaintainerPreparesRealSummary(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-maint"
	seedWithApp(t, bus, sid, "app1", 12)

	llm := &fakeLLM{reply: "BG-SUMMARY-TEXT"}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())
	m := newSummaryMaintainer(bus, c, llm, discardLog())

	m.prepare(sid)

	if llm.callCount() == 0 {
		t.Fatal("maintainer must call the LLM to produce the summary (off the loop)")
	}
	snap := mustSnap(t, bus, sid)
	if snap.PreparedSummary == nil {
		t.Fatal("no prepared summary after maintainer.prepare")
	}
	if !strings.Contains(snap.PreparedSummary.Summary, "BG-SUMMARY-TEXT") {
		t.Errorf("prepared summary = %q, want it to contain the LLM output", snap.PreparedSummary.Summary)
	}
	if snap.PreparedSummary.CoversSeq == 0 {
		t.Error("prepared CoversSeq not set")
	}
}

// TestCTX8_MaintainerNoPrepareOnLLMFailure: an LLM failure must NOT produce a
// prepared candidate (the gate truncates instantly instead).
func TestCTX8_MaintainerNoPrepareOnLLMFailure(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-maint-fail"
	seedWithApp(t, bus, sid, "app1", 12)

	llm := &fakeLLM{err: errors.New("provider down")}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())
	m := newSummaryMaintainer(bus, c, llm, discardLog())

	m.prepare(sid)
	if snap := mustSnap(t, bus, sid); snap.PreparedSummary != nil {
		t.Errorf("LLM failure must not prepare a candidate, got %+v", snap.PreparedSummary)
	}
}

// TestCTX8_MaintainerHysteresis: re-preparing the same aged region must not call
// the LLM again (no churn).
func TestCTX8_MaintainerHysteresis(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-hyst"
	seedWithApp(t, bus, sid, "app1", 12)

	llm := &fakeLLM{reply: "S"}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())
	m := newSummaryMaintainer(bus, c, llm, discardLog())

	m.prepare(sid)
	first := llm.callCount()
	if first == 0 {
		t.Fatal("first prepare did not call the LLM")
	}
	m.prepare(sid) // same messages → covered already → no new LLM call
	if got := llm.callCount(); got != first {
		t.Errorf("maintainer re-summarised the same region (no hysteresis): %d → %d calls", first, got)
	}
}

// TestCTX8_EndToEndPrepareThenApply: the maintainer prepares, then the gate
// applies the prepared summary instantly with no further LLM call.
func TestCTX8_EndToEndPrepareThenApply(t *testing.T) {
	bus := newCompactorTestBus(t)
	sid := "ctx8-e2e"
	seedWithApp(t, bus, sid, "app1", 14)

	llm := &fakeLLM{reply: "E2E-SUMMARY"}
	c := newContextCompactor(bus, appsWithBrain(), llm, discardLog())
	c.nonBlocking = true
	m := newSummaryMaintainer(bus, c, llm, discardLog())

	m.prepare(sid) // background: one LLM call, prepares the candidate
	afterPrepare := llm.callCount()

	if err := c.CompactSession(context.Background(), sid, "summarize", 4); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if got := llm.callCount(); got != afterPrepare {
		t.Errorf("the gate made an extra LLM call (%d → %d) — it must apply the prepared one", afterPrepare, got)
	}
	snap := mustSnap(t, bus, sid)
	if snap.ContextCompaction == nil || snap.ContextCompaction.Strategy != "summarize" {
		t.Fatalf("gate did not apply the prepared summary: %+v", snap.ContextCompaction)
	}
	if !strings.Contains(snap.ContextCompaction.Summary, "E2E-SUMMARY") {
		t.Errorf("applied summary = %q, want the prepared high-fidelity one", snap.ContextCompaction.Summary)
	}
}
