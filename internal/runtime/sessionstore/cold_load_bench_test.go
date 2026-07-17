package sessionstore

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

var osMkdirAll = os.MkdirAll

func writeSessionEvents(b *testing.B, paths Paths, sid string, count int) {
	b.Helper()
	if err := osMkdirAll(paths.SessionDir(sid), 0o700); err != nil {
		b.Fatalf("mkdir session dir: %v", err)
	}
	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		b.Fatalf("open jsonl: %v", err)
	}
	defer w.Close()

	now := time.Now().UnixNano()
	events := make([]Event, 0, count)
	for i := 0; i < count; i++ {
		ev := Event{
			Seq:        uint64(i + 1),
			TsUnixNano: now + int64(i),
			SessionID:  sid,
			AppID:      "bench-app",
			UserID:     "bench-user",
		}
		switch i % 5 {
		case 0:
			ev.Type = EventUserMessage
			ev.Message = &MessagePayload{
				Role: "user",
				Parts: []MessagePart{
					{Type: PartTypeText, Text: fmt.Sprintf("user message #%d with some realistic length text", i)},
				},
			}
		case 1:
			ev.Type = EventTurnStarted
			ev.Turn = &TurnPayload{TurnID: fmt.Sprintf("turn-%d", i)}
		case 2:
			ev.Type = EventAssistantMessage
			ev.Message = &MessagePayload{
				Role: "assistant",
				Parts: []MessagePart{
					{Type: PartTypeText, Text: fmt.Sprintf("assistant reply #%d, including reasoning steps and a detailed answer to the user question above. This makes the messages bigger to test the realistic projection cost.", i)},
				},
			}
		case 3:
			ev.Type = EventToolCall
			ev.Tool = &ToolPayload{
				CallID:    fmt.Sprintf("call-%d", i),
				Name:      "web_search",
				Arguments: map[string]any{"q": fmt.Sprintf("query %d", i)},
				Status:    "pending",
			}
		case 4:
			ev.Type = EventTurnEnded
			ev.Turn = &TurnPayload{TurnID: fmt.Sprintf("turn-%d", i), Status: "done"}
		}
		events = append(events, ev)
		if len(events) >= 200 {
			if _, err := w.Write(events); err != nil {
				b.Fatalf("write batch: %v", err)
			}
			events = events[:0]
		}
	}
	if len(events) > 0 {
		if _, err := w.Write(events); err != nil {
			b.Fatalf("write tail: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		b.Fatalf("flush: %v", err)
	}
}

func BenchmarkColdLoad_1000Events(b *testing.B) {
	dir := b.TempDir()
	paths := NewPaths(dir)
	sid := "bench-session"
	writeSessionEvents(b, paths, sid, 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
		if err != nil {
			b.Fatalf("load: %v", err)
		}
		if res.EventsApplied < 1000 {
			b.Fatalf("expected 1000 events applied, got %d", res.EventsApplied)
		}
	}
}

func BenchmarkColdLoad_5000Events(b *testing.B) {
	dir := b.TempDir()
	paths := NewPaths(dir)
	sid := "bench-session-big"
	writeSessionEvents(b, paths, sid, 5000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
		if err != nil {
			b.Fatalf("load: %v", err)
		}
		if res.EventsApplied < 5000 {
			b.Fatalf("expected 5000 events applied, got %d", res.EventsApplied)
		}
	}
}

func BenchmarkColdLoad_Concurrent100Sessions(b *testing.B) {
	const N = 100
	dir := b.TempDir()
	paths := NewPaths(dir)

	for i := 0; i < N; i++ {
		writeSessionEvents(b, paths, fmt.Sprintf("sess-%d", i), 100)
	}

	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		var wg sync.WaitGroup
		errs := make(chan error, N)
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := Load(paths, fmt.Sprintf("sess-%d", i), LoadOptions{Mode: JSONLBestEffort}); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			b.Fatal(err)
		}
	}
}

func BenchmarkProjectionOnly(b *testing.B) {
	const N = 1000
	now := time.Now().UnixNano()
	events := make([]Event, N)
	for i := 0; i < N; i++ {
		events[i] = Event{
			Seq:        uint64(i + 1),
			TsUnixNano: now + int64(i),
			SessionID:  "p",
			Type:       EventUserMessage,
			Message:    &MessagePayload{Role: "user", Parts: []MessagePart{{Type: PartTypeText, Text: "hello"}}},
		}
	}
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		st := NewSessionState("p")
		st.mu.Lock()
		for i := range events {
			applyLocked(st, &events[i])
		}
		st.mu.Unlock()
	}
}
