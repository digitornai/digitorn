//go:build live

package runtime_test

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// LL-2 TOOL ROUTING — verify the LLM picks the right tools given
// a natural language instruction.
// =====================================================================

// TestLive_RoutingSingleRead : "Read hello.txt" must trigger
// filesystem.read with the right path.
func TestLive_RoutingSingleRead(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "hello.txt", "this is the secret content")
	f.runLive(t, "Read the file hello.txt and tell me what it says.")

	assertToolCalled(t, f, "filesystem.read")
	assertSemantic(t, f, "secret content")
}

// TestLive_RoutingReadAndAct : LLM must read the file AND use the
// content in its reply (no hallucination).
func TestLive_RoutingReadAndAct(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "color.txt", "magenta")
	f.runLive(t, "What color is mentioned in color.txt ?")

	assertToolCalled(t, f, "filesystem.read")
	assertSemantic(t, f, "magenta")
	// And must NOT hallucinate other colors.
	assertSemanticNotIn(t, f, "blue", "green", "red")
}

// TestLive_RoutingNotExistRecovers : the LLM tries to read a
// non-existent file ; the runtime returns an error ; the LLM must
// then explain it to the user instead of crashing.
func TestLive_RoutingNotExistRecovers(t *testing.T) {
	f := liveSetup(t)
	// no file created
	f.runLive(t, "Read the file 'definitely-not-there.txt' and tell me what it says.")

	assertToolCalled(t, f, "filesystem.read")
	// Reply must mention something like "not found", "doesn't exist", "error", etc.
	got := finalAssistantText(f)
	low := strings.ToLower(got)
	if !(strings.Contains(low, "not") || strings.Contains(low, "exist") ||
		strings.Contains(low, "error") || strings.Contains(low, "fail")) {
		t.Errorf("expected error explanation, got : %q", got)
	}
}

// TestLive_RoutingLsThenRead : list a directory, pick a file, read
// it. Exercises chained tool calls in one turn.
func TestLive_RoutingLsThenRead(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "alpha.txt", "the alpha content")
	f.writeWorkspaceFile(t, "beta.txt", "the beta content")
	f.runLive(t, "List the files in the workspace, then read alpha.txt.")

	if !toolWasCalled(f, "filesystem.ls") {
		t.Logf("filesystem.ls was not called (LLM may have skipped to read directly)")
	}
	assertToolCalled(t, f, "filesystem.read")
	assertSemantic(t, f, "alpha")
}

// TestLive_RoutingGrep : search for a regex pattern in files.
func TestLive_RoutingGrep(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "log.txt", "INFO: ok\nERROR: something broke\nINFO: ok again")
	f.runLive(t, "Search the file log.txt for any line containing ERROR. Tell me what you find.")

	if !toolWasCalled(f, "filesystem.grep") && !toolWasCalled(f, "filesystem.read") {
		t.Error("expected either grep or read to be called")
	}
	assertSemantic(t, f, "error", "broke", "something")
}

// TestLive_RoutingWriteThenRead : LLM writes a file then reads it
// back to verify. Two tool calls in sequence.
func TestLive_RoutingWriteThenRead(t *testing.T) {
	f := liveSetup(t)
	f.runLive(t, "Write the word 'banana' to a file called fruit.txt, then read it back to confirm.")

	assertToolCalled(t, f, "filesystem.write")
	if !toolWasCalled(f, "filesystem.read") {
		t.Logf("note : LLM didn't read back (some models skip the verify step)")
	}
	assertSemantic(t, f, "banana")
}

// TestLive_RoutingParallel : "read both a.txt and b.txt" should
// either result in two filesystem.read calls or a single
// run_parallel call. Either is acceptable per the doc.
func TestLive_RoutingParallel(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "a.txt", "content A")
	f.writeWorkspaceFile(t, "b.txt", "content B")
	f.runLive(t, "Read both a.txt and b.txt and tell me what's in each.")

	totalReads := countToolCalls(f, "filesystem.read")
	parallelCalls := countToolCalls(f, "context_builder.run_parallel")
	if totalReads < 2 && parallelCalls == 0 {
		t.Errorf("expected 2 reads or 1 run_parallel, got %d reads / %d parallel",
			totalReads, parallelCalls)
	}
	assertSemantic(t, f, "content a", "content A")
	assertSemantic(t, f, "content b", "content B")
}

// TestLive_RoutingNoReadWhenAnswerIsObvious : "What's the capital
// of France ?" should NOT trigger any filesystem tool.
func TestLive_RoutingNoReadWhenAnswerIsObvious(t *testing.T) {
	f := liveSetup(t)
	f.runLive(t, "What's the capital of France ? Just the name.")

	for _, tn := range []string{
		"filesystem.read", "filesystem.write",
		"filesystem.ls", "filesystem.grep",
	} {
		assertToolNotCalled(t, f, tn)
	}
	assertSemantic(t, f, "paris")
}

// TestLive_RoutingResultPropagation : after a tool call, the
// result must reach the LLM's next round so its reply uses it.
// This indirectly tests the projection layer.
func TestLive_RoutingResultPropagation(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "secret.txt", "supercalifragilisticexpialidocious")
	f.runLive(t, "Read secret.txt and repeat back the EXACT word inside.")

	assertToolCalled(t, f, "filesystem.read")
	assertSemantic(t, f, "supercalifragilisticexpialidocious")
}

// TestLive_RoutingPartialFailure : LLM tries to read 2 files,
// one exists, the other doesn't. The LLM must report on both
// (success for one, error for the other).
func TestLive_RoutingPartialFailure(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "exists.txt", "I exist")
	f.runLive(t, "Read both exists.txt and missing.txt and report on each separately.")

	// At least one read call must have happened.
	if countToolCalls(f, "filesystem.read") == 0 &&
		countToolCalls(f, "context_builder.run_parallel") == 0 {
		t.Error("no read or run_parallel called")
	}
	// Sanity : assistant message must mention both filenames or
	// the contents of the existing one.
	assertSemantic(t, f, "i exist", "exists.txt", "missing")
}

// =====================================================================
// Additional sanity : per-turn tool counts are persisted as
// EventToolCall and EventToolResult pairs.
// =====================================================================

// TestLive_RoutingEventsPersisted : after a tool call, the session
// must have both EventToolCall and EventToolResult, and they must
// share a CallID.
func TestLive_RoutingEventsPersisted(t *testing.T) {
	f := liveSetup(t)
	f.writeWorkspaceFile(t, "note.txt", "ping")
	f.runLive(t, "Read note.txt.")

	var calls, results []string
	for _, ev := range f.session.events {
		if ev.Tool == nil {
			continue
		}
		switch ev.Type {
		case sessionstore.EventToolCall:
			calls = append(calls, ev.Tool.CallID)
		case sessionstore.EventToolResult:
			results = append(results, ev.Tool.CallID)
		}
	}
	if len(calls) == 0 {
		t.Fatal("no EventToolCall persisted")
	}
	if len(results) == 0 {
		t.Fatal("no EventToolResult persisted")
	}
	// Every result CallID must appear in calls (and vice versa
	// for completed turns).
	callSet := map[string]bool{}
	for _, id := range calls {
		callSet[id] = true
	}
	for _, id := range results {
		if !callSet[id] {
			t.Errorf("EventToolResult.CallID %q has no matching EventToolCall", id)
		}
	}
}
