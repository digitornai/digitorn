//go:build live

package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
)

// =====================================================================
// LL-1 SMOKE — basic plumbing of LLM gateway, no tool involvement
// =====================================================================

// TestLive_SmokeReplies : LLM returns a greeting. Verifies the end-
// to-end gRPC plumbing : runtime → llm.Client → worker → gateway →
// provider → back. Asserts the assistant message landed and is
// non-empty.
func TestLive_SmokeReplies(t *testing.T) {
	f := liveSetup(t)
	f.runLive(t, "Say hello in exactly 3 words.")

	text := finalAssistantText(f)
	if text == "" {
		t.Fatal("no assistant text returned")
	}
	if !strings.Contains(strings.ToLower(text), "hello") {
		t.Errorf("expected 'hello' in reply, got : %q", text)
	}
}

// TestLive_SmokeNoToolNeeded : a math question that the LLM should
// answer directly without calling any tool. Verifies the model
// doesn't hallucinate tool calls when none are warranted.
func TestLive_SmokeNoToolNeeded(t *testing.T) {
	f := liveSetup(t)
	f.runLive(t, "What is 2 + 2 ? Just say the number.")

	for toolName := range map[string]bool{
		"filesystem.read": true, "filesystem.write": true,
		"filesystem.ls": true, "filesystem.grep": true,
	} {
		assertToolNotCalled(t, f, toolName)
	}
	assertSemantic(t, f, "4", "four")
}

// TestLive_SmokeEmptyPrompt : an effectively-empty user message
// must not crash the runtime. The LLM may apologise ; that's fine.
// What matters is the turn completes.
func TestLive_SmokeEmptyPrompt(t *testing.T) {
	f := liveSetup(t)
	f.runLive(t, "")
	// No tool calls expected ; assistant must still produce SOME
	// output (even an apology counts).
	got := finalAssistantText(f)
	if got == "" {
		t.Error("LLM produced no reply for empty prompt")
	}
}

// TestLive_SmokeLongPrompt : a 5KB prompt. Verifies the worker
// handles non-trivial input sizes without truncation issues.
func TestLive_SmokeLongPrompt(t *testing.T) {
	f := liveSetup(t)
	long := strings.Repeat("Here is some filler content. ", 200) +
		"\n\nNow reply with exactly the word 'acknowledged'."
	f.runLive(t, long)
	assertSemantic(t, f, "acknowledged")
}

// TestLive_SmokeCancellation : interrupt the turn while the LLM is
// generating. The engine must return a cancellation error and emit
// EventTurnEnded with status=interrupted (RT-6).
func TestLive_SmokeCancellation(t *testing.T) {
	f := liveSetup(t)
	f.injectUser(t, "Write a 1000-word essay about the history of computing.")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := f.engine.Run(ctx, dgruntime.TurnInput{
			AppID: "live-app", SessionID: "live-sess", UserID: "test-user",
			UserJWT: f.userJWT,
		})
		done <- result{err}
	}()

	// Cancel immediately to interrupt.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("expected cancellation error")
		}
		if !strings.Contains(strings.ToLower(r.err.Error()), "context") &&
			!strings.Contains(strings.ToLower(r.err.Error()), "cancel") {
			t.Errorf("error doesn't look like cancellation : %v", r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
