package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type interruptStreamStub struct {
	*stubLLM
	deltas []string
}

func (s *interruptStreamStub) ChatStream(ctx context.Context, _ *llm.ChatRequest) (<-chan *llm.ChatChunk, error) {
	out := make(chan *llm.ChatChunk)
	go func() {
		defer close(out)
		for _, d := range s.deltas {
			select {
			case <-ctx.Done():
				return
			case out <- &llm.ChatChunk{Delta: d}:
			}
		}
		<-ctx.Done()
	}()
	return out, nil
}

func TestAbort_StreamingSavesPartialAnswer(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	stub := &interruptStreamStub{
		stubLLM: &stubLLM{resp: &llm.ChatResponse{Content: "sync fallback"}},
		deltas:  []string{"The answer ", "is partial"},
	}
	e := newEngine(t, apps, sess, stub)
	e.Streaming = true

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-1", UserID: "u"})
		done <- err
	}()

	waitFor(t, func() bool { return sess.count(sessionstore.EventAssistantDelta) >= 2 }, "deltas streamed")
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("aborted turn must return an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not unwind promptly after abort — streaming not interrupted")
	}

	var partial string
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventAssistantMessage && ev.Message != nil {
			partial = ev.Message.Content
		}
	}
	if !strings.Contains(partial, "The answer is partial") {
		t.Errorf("partial streamed answer not saved durably, got %q", partial)
	}

	var sawInterrupt bool
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventTurnEnded && ev.Turn != nil && ev.Turn.Status == "interrupted" {
			sawInterrupt = true
		}
	}
	if !sawInterrupt {
		t.Error("turn must close with status=interrupted")
	}
}

func TestAbort_ResumeWithDanglingToolCall_BuildsValidPrompt(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	mustAppend := func(ev sessionstore.Event) {
		ev.SessionID, ev.AppID, ev.UserID = "sess-1", "rt3-app", "u"
		if _, err := sess.AppendDurable(context.Background(), ev); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	mustAppend(sessionstore.Event{Type: sessionstore.EventUserMessage, Message: &sessionstore.MessagePayload{
		Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "read hello.txt"}}}})
	mustAppend(sessionstore.Event{Type: sessionstore.EventAssistantMessage, Message: &sessionstore.MessagePayload{
		Role: "assistant", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "c1", Name: "filesystem.read"}},
		}}})
	mustAppend(sessionstore.Event{Type: sessionstore.EventUserMessage, Message: &sessionstore.MessagePayload{
		Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "never mind — just say hi"}}}})

	lc := &stubLLM{responses: []*llm.ChatResponse{{Content: "hi"}}}
	e := newEngine(t, apps, sess, lc)

	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-1", UserID: "u"}); err != nil {
		t.Fatalf("resume turn failed: %v", err)
	}

	if lc.got == nil {
		t.Fatal("LLM was not called on resume")
	}
	var sawCall, sawResult bool
	for _, m := range lc.got.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID == "c1" {
				sawCall = true
			}
		}
		if m.Role == "tool" && m.ToolCallID == "c1" {
			sawResult = true
		}
	}
	if !sawCall {
		t.Fatal("assistant tool_call lost from the resume prompt")
	}
	if !sawResult {
		t.Error("DANGLING tool_call reached the LLM unpaired — resume prompt is malformed")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within timeout: %s", what)
}
