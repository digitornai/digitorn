package sessionstore

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestColdLoad_1000Events_UnderBudget is a regression guard, not a
// benchmark : it asserts that cold-loading a 1000-event session
// completes well below the budget every user experiences as "instant"
// when they reopen a conversation.
//
// Budget : 100ms wall-clock on commodity hardware. The current
// implementation lands around 8ms ; if a refactor pushes it over 100ms
// the CI breaks and we know to investigate.
func TestColdLoad_1000Events_UnderBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test : run without -short")
	}
	dir := t.TempDir()
	paths := NewPaths(dir)
	sid := "regression-session"

	// Materialise 1000 realistic events on disk.
	if err := os.MkdirAll(paths.SessionDir(sid), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	for i := 0; i < 1000; i++ {
		ev := Event{
			Seq:        uint64(i + 1),
			TsUnixNano: now + int64(i),
			SessionID:  sid,
			AppID:      "regression-app",
			UserID:     "regression-user",
		}
		switch i % 4 {
		case 0:
			ev.Type = EventUserMessage
			ev.Message = &MessagePayload{Role: "user", Parts: []MessagePart{{Type: PartTypeText, Text: fmt.Sprintf("message %d with realistic length", i)}}}
		case 1:
			ev.Type = EventTurnStarted
			ev.Turn = &TurnPayload{TurnID: fmt.Sprintf("turn-%d", i)}
		case 2:
			ev.Type = EventAssistantMessage
			ev.Message = &MessagePayload{Role: "assistant", Parts: []MessagePart{{Type: PartTypeText, Text: fmt.Sprintf("assistant reply %d with multi-sentence response demonstrating realistic chat content size", i)}}}
		case 3:
			ev.Type = EventTurnEnded
			ev.Turn = &TurnPayload{TurnID: fmt.Sprintf("turn-%d", i), Status: "done"}
		}
		if _, err := w.Write([]Event{ev}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Cold-load with a generous budget. Local SSD lands ~8ms ; CI on
	// slow disk runners may hit 30-40ms. 100ms catches actual
	// regressions (algorithmic, allocation explosion) without being
	// flaky on slow CI.
	const budget = 100 * time.Millisecond

	start := time.Now()
	res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("load failed : %v", err)
	}
	if res.EventsApplied != 1000 {
		t.Fatalf("expected 1000 events applied, got %d", res.EventsApplied)
	}
	if elapsed > budget {
		t.Fatalf("cold load took %v, budget %v — PERF REGRESSION", elapsed, budget)
	}
	t.Logf("cold load 1000 events : %v (budget %v)", elapsed, budget)
}
