package meta_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// recordingInner captures the canonical tool names run_parallel dispatched, so
// a test can assert the liberal parser routed every task correctly.
type recordingInner struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingInner) Dispatch(_ context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	r.mu.Lock()
	r.seen = append(r.seen, c.Name)
	r.mu.Unlock()
	return runtime.ToolOutcome{Status: "completed", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}}
}

func (r *recordingInner) sorted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.seen...)
	sort.Strings(out)
	return out
}

// TestRunParallel_LiberalShapes is the anti-"burg" guard : a primitive this
// central must accept every reasonable way a model passes the task list, never
// failing on shape alone. Each sub-case feeds a different-but-valid envelope and
// asserts BOTH tasks dispatched with the right canonical names.
func TestRunParallel_LiberalShapes(t *testing.T) {
	twoTasks := func(toolKey, argsKey string) []any {
		return []any{
			map[string]any{toolKey: "m.read", argsKey: map[string]any{"path": "a"}},
			map[string]any{toolKey: "m.write", argsKey: map[string]any{"path": "b"}},
		}
	}
	want := []string{"m.read", "m.write"}

	cases := []struct {
		name string
		args map[string]any
	}{
		{"canonical tasks/{tool,args}", map[string]any{"tasks": twoTasks("tool", "args")}},
		{"legacy actions/{name,params}", map[string]any{"actions": twoTasks("name", "params")}},
		{"bare array (decodeArgs sentinel)", map[string]any{llm.ArgsArrayKey: twoTasks("tool", "args")}},
		{"alias calls/{tool,arguments}", map[string]any{"calls": twoTasks("tool", "arguments")}},
		{"sole array under a random key", map[string]any{"whatever": twoTasks("tool", "args")}},
		{"string-encoded array under tasks (LLM double-encode)", map[string]any{"tasks": `[{"tool":"m.read","args":{"path":"a"}},{"tool":"m.write","args":{"path":"b"}}]`}},
		{"string-encoded bare array under sentinel", map[string]any{llm.ArgsArrayKey: `[{"tool":"m.read","args":{"path":"a"}},{"tool":"m.write","args":{"path":"b"}}]`}},
		{"mixed item keys", map[string]any{"tasks": []any{
			map[string]any{"name": "m.read", "params": map[string]any{"path": "a"}},
			map[string]any{"tool": "m.write", "args": map[string]any{"path": "b"}},
		}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingInner{}
			disp := &meta.MetaDispatcher{Inner: rec}
			out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
				Name: "context_builder.run_parallel",
				Args: c.args,
			})
			if out.Status != "completed" {
				t.Fatalf("status = %q, error = %q", out.Status, out.Error)
			}
			got := rec.sorted()
			if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("dispatched %v, want %v", got, want)
			}
		})
	}
}

// TestRunParallel_EmptyStillErrors : liberal does NOT mean silent — a genuinely
// empty/garbage call must still return a clear, actionable error.
func TestRunParallel_EmptyStillErrors(t *testing.T) {
	disp := &meta.MetaDispatcher{Inner: &recordingInner{}}
	out := disp.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.run_parallel",
		Args: map[string]any{},
	})
	if out.Status != "errored" {
		t.Fatalf("empty run_parallel must error, got %q", out.Status)
	}
}
