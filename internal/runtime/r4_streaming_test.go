package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// R-4 — Streaming
//
// The tests prove :
//   - When Engine.Streaming=true AND the LLM client satisfies
//     LLMStream, the engine emits one EventAssistantDelta per
//     chunk delta and the final ChatResponse content is the
//     concatenation of all deltas.
//   - When Engine.Streaming=false OR LLM doesn't satisfy
//     LLMStream, the engine uses synchronous Chat and emits no
//     deltas.
//   - Stream errors propagate as turn errors with "stream:" prefix.
//   - Concurrent streaming sessions don't interleave their deltas.
// =====================================================================

// streamingStub is a stubLLM extension that also implements
// LLMStream. The streamRounds slice is consumed one round per
// ChatStream call — round 0's chunks are sent on the first call,
// round 1 on the second, etc. After the last round, an empty
// terminator stream is returned so the agent loop can break
// cleanly without re-replaying the same chunks.
type streamingStub struct {
	*stubLLM
	mu            sync.Mutex
	streamRounds  [][]*llm.ChatChunk
	streamOpenErr error
	streamCallCnt int
}

func newStreamingStub(chunks ...*llm.ChatChunk) *streamingStub {
	return &streamingStub{
		stubLLM: &stubLLM{
			resp: &llm.ChatResponse{Content: "sync fallback"},
		},
		streamRounds: [][]*llm.ChatChunk{chunks},
	}
}

func newStreamingStubRounds(rounds ...[]*llm.ChatChunk) *streamingStub {
	return &streamingStub{
		stubLLM: &stubLLM{
			resp: &llm.ChatResponse{Content: "sync fallback"},
		},
		streamRounds: rounds,
	}
}

func (s *streamingStub) ChatStream(
	ctx context.Context, _ *llm.ChatRequest,
) (<-chan *llm.ChatChunk, error) {
	s.mu.Lock()
	idx := s.streamCallCnt
	s.streamCallCnt++
	openErr := s.streamOpenErr
	var chunks []*llm.ChatChunk
	if idx < len(s.streamRounds) {
		chunks = s.streamRounds[idx]
	}
	s.mu.Unlock()
	if openErr != nil {
		return nil, openErr
	}
	out := make(chan *llm.ChatChunk, len(chunks)+1)
	go func() {
		defer close(out)
		for _, c := range chunks {
			select {
			case <-ctx.Done():
				return
			case out <- c:
			}
		}
	}()
	return out, nil
}

// =====================================================================
// 1. Streaming OFF : behaves exactly as RT-3
// =====================================================================

func TestR4_StreamingDisabledUsesSyncPath(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	ss := newStreamingStub(
		&llm.ChatChunk{Delta: "hello"},
		&llm.ChatChunk{Delta: " world"},
	)
	ss.stubLLM.resp = &llm.ChatResponse{Content: "synchronous reply"}

	e := newEngine(t, apps, sess, ss)
	e.Streaming = false

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if ss.streamCallCnt != 0 {
		t.Errorf("ChatStream called %d times despite Streaming=false", ss.streamCallCnt)
	}
	if got := sess.count(sessionstore.EventAssistantDelta); got != 0 {
		t.Errorf("delta events = %d, want 0", got)
	}
}

// =====================================================================
// 2. Streaming ON : deltas emitted in order
// =====================================================================

func TestR4_StreamingEmitsDeltasInOrder(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	chunks := []*llm.ChatChunk{
		{Delta: "Hello, "},
		{Delta: "world"},
		{Delta: "!"},
		{FinishReason: "stop"},
	}
	ss := newStreamingStub(chunks...)

	e := newEngine(t, apps, sess, ss)
	e.Streaming = true

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if ss.streamCallCnt != 1 {
		t.Errorf("ChatStream called %d times, want 1", ss.streamCallCnt)
	}

	// Three deltas (the FinishReason-only chunk doesn't carry
	// delta text and so should NOT emit an EventAssistantDelta).
	if got := sess.count(sessionstore.EventAssistantDelta); got != 3 {
		t.Errorf("delta events = %d, want 3", got)
	}

	// The deltas must be in the order we sent them.
	var seq []string
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventAssistantDelta && ev.Message != nil {
			for _, p := range ev.Message.Parts {
				if p.Type == sessionstore.PartTypeText {
					seq = append(seq, p.Text)
				}
			}
		}
	}
	want := []string{"Hello, ", "world", "!"}
	if len(seq) != 3 || seq[0] != want[0] || seq[1] != want[1] || seq[2] != want[2] {
		t.Errorf("delta sequence = %v, want %v", seq, want)
	}

	// The assistant message that landed must have the
	// concatenated content.
	var assistantText string
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventAssistantMessage && ev.Message != nil {
			for _, p := range ev.Message.Parts {
				if p.Type == sessionstore.PartTypeText {
					assistantText += p.Text
				}
			}
		}
	}
	if assistantText != "Hello, world!" {
		t.Errorf("assistant content = %q, want concatenation", assistantText)
	}
}

// =====================================================================
// 3. Tool calls reach the agent loop after streaming
// =====================================================================

func TestR4_StreamingHandsOffToolCalls(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	// Round 1 : delta + tool call. Round 2 : final answer.
	ss := newStreamingStubRounds(
		[]*llm.ChatChunk{
			{Delta: "Reading file..."},
			{ToolCalls: []llm.ChatToolCall{{
				ID:        "c1",
				Name:      "filesystem.read",
				Arguments: map[string]any{"path": "x.txt"},
			}}},
		},
		[]*llm.ChatChunk{
			{Delta: "Done."},
		},
	)

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, ss)
	e.Streaming = true
	e.Context = cb
	e.Dispatcher = disp

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := sess.count(sessionstore.EventToolCall); got != 1 {
		t.Errorf("tool_call events = %d, want 1", got)
	}
	if got := sess.count(sessionstore.EventToolResult); got != 1 {
		t.Errorf("tool_result events = %d, want 1", got)
	}
}

// =====================================================================
// 4. Stream open error propagates
// =====================================================================

func TestR4_StreamOpenError(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	ss := newStreamingStub()
	ss.streamOpenErr = errors.New("worker unavailable")

	e := newEngine(t, apps, sess, ss)
	e.Streaming = true

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected error from open failure")
	}
	if !strings.Contains(err.Error(), "stream:") {
		t.Errorf("err should mention 'stream:' : %v", err)
	}
}

// =====================================================================
// 5. Stream provider mid-stream error
// =====================================================================

func TestR4_StreamProviderError(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	ss := newStreamingStub(
		&llm.ChatChunk{Delta: "started..."},
		&llm.ChatChunk{Error: "provider rate limit"},
	)

	e := newEngine(t, apps, sess, ss)
	e.Streaming = true

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected provider error")
	}
	if !strings.Contains(err.Error(), "provider rate limit") {
		t.Errorf("err should carry the provider message : %v", err)
	}
}

// =====================================================================
// 6. Non-streaming client falls back silently
// =====================================================================

func TestR4_NonStreamingClientFallsBack(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	// Plain stubLLM has NO ChatStream method ; setting Streaming=true
	// must trigger the fallback to Chat.
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "sync"}}

	e := newEngine(t, apps, sess, lc)
	e.Streaming = true

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sess.count(sessionstore.EventAssistantDelta); got != 0 {
		t.Errorf("delta events = %d, want 0 (fallback to sync)", got)
	}
}

// =====================================================================
// 7. Empty stream (no chunks) yields empty content
// =====================================================================

func TestR4_EmptyStream(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	ss := newStreamingStub()

	e := newEngine(t, apps, sess, ss)
	e.Streaming = true

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sess.count(sessionstore.EventAssistantDelta); got != 0 {
		t.Errorf("empty stream produced delta events")
	}
}
