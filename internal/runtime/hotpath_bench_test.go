package runtime_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	runtimepkg "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/turn"
)

// BenchmarkRuntime_FullTurn measures a REAL completed turn (1 LLM → 1 tool
// → 1 LLM), unlike BenchmarkAgentLoop_OneRoundOneCall which omits the Pool
// and so aborts at turn.New. It wires an unbounded Pool and ASSERTS the run
// succeeds, so the profile reflects the whole hot path: convert → dispatch
// → persist → re-convert.
func BenchmarkRuntime_FullTurn(b *testing.B) {
	apps := &stubApps{app: benchApp()}
	disp := &runtimepkg.StaticToolDispatcher{Outcomes: map[string]runtimepkg.ToolOutcome{
		"t": {Status: "completed", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}},
	}}
	pool := turn.NewPool(turn.PoolConfig{})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess := newProjectingSessions(fmt.Sprintf("s-%d", i))
		lc := &stubLLM{responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "t"}}},
			{Content: "done"},
		}}
		e := &runtimepkg.Engine{
			Apps: apps, Sessions: sess, LLM: lc, Dispatcher: disp,
			Pool:   pool,
			IDGen:  benchIDGen(),
			Logger: discardLogger(),
		}
		if _, err := e.Run(context.Background(), runtimepkg.TurnInput{
			AppID: "app-1", SessionID: fmt.Sprintf("s-%d", i), UserID: "u",
		}); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}
