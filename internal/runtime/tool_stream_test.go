package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/turn"
)

// capturingSessions is a minimal SessionAccess that records appended events so a
// test can assert what the engine emitted.
type capturingSessions struct {
	mu     sync.Mutex
	events []sessionstore.Event
}

func (c *capturingSessions) State(string) (*sessionstore.SessionState, error) { return nil, nil }

func (c *capturingSessions) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return uint64(len(c.events)), nil
}

// A streaming tool call must surface the tool NAME on the first fragment and a
// token counter that GROWS as the arguments stream in — keyed by the SAME
// call_id so the client updates one chip. This is what makes a long write
// visible instead of leaving the client blind.
func TestEmitToolCallStreamDeltas_NameThenGrowingTokens(t *testing.T) {
	cap := &capturingSessions{}
	e := &Engine{Sessions: cap}
	tr := &turn.Turn{ID: "corr"}
	in := TurnInput{SessionID: "s", AppID: "a", UserID: "u"}
	live := map[int]*liveToolCall{}

	// Fragment 1 : id + name + a little of the args (running output count = 12).
	e.emitToolCallStreamDeltas(context.Background(), tr, in, live, []llm.ChatToolCallDelta{
		{Index: 0, ID: "call_1", Name: "filesystem.write", ArgsChars: 8},
	}, 12)
	// Fragment 2 : a big slice of args, no id/name repeated (running count = 120).
	e.emitToolCallStreamDeltas(context.Background(), tr, in, live, []llm.ChatToolCallDelta{
		{Index: 0, ArgsChars: 400},
	}, 120)

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.events) != 2 {
		t.Fatalf("expected 2 streaming events, got %d", len(cap.events))
	}
	first, last := cap.events[0].Tool, cap.events[1].Tool
	if first == nil || first.Status != "streaming" ||
		first.Name != "filesystem.write" || first.CallID != "call_1" {
		t.Fatalf("first streaming event wrong: %+v", first)
	}
	if last.CallID != "call_1" || last.Name != "filesystem.write" {
		t.Fatalf("id/name not carried onto later frame: %+v", last)
	}
	if last.LiveTokens <= first.LiveTokens {
		t.Fatalf("token counter did not grow: %d -> %d", first.LiveTokens, last.LiveTokens)
	}
	// The running output count (text + tool args) rides LiveOutputTokens so the
	// client's single central counter includes tool-call tokens.
	if cap.events[0].LiveOutputTokens != 12 || cap.events[1].LiveOutputTokens != 120 {
		t.Fatalf("LiveOutputTokens not carried: %d, %d (want 12, 120)",
			cap.events[0].LiveOutputTokens, cap.events[1].LiveOutputTokens)
	}
}

// Some providers send argument bytes before the call id : nothing is emitted
// until the id is known, so the client can key the chip stably.
func TestEmitToolCallStreamDeltas_NoEmitUntilID(t *testing.T) {
	cap := &capturingSessions{}
	e := &Engine{Sessions: cap}
	live := map[int]*liveToolCall{}
	e.emitToolCallStreamDeltas(context.Background(), &turn.Turn{ID: "c"},
		TurnInput{SessionID: "s", AppID: "a"}, live,
		[]llm.ChatToolCallDelta{{Index: 0, ArgsChars: 50}}, 12)
	if len(cap.events) != 0 {
		t.Fatalf("must not emit before the call id is known, got %d", len(cap.events))
	}
}
