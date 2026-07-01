package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// taskIDFakeSessions is a SessionAccess whose State carries no todos, so
// nextTaskID seeds its counter at 0 and must still hand out unique ids.
type taskIDFakeSessions struct{}

func (taskIDFakeSessions) State(string) (*sessionstore.SessionState, error) { return nil, nil }
func (taskIDFakeSessions) AppendDurable(context.Context, sessionstore.Event) (uint64, error) {
	return 0, nil
}
func (taskIDFakeSessions) Append(context.Context, sessionstore.Event) (uint64, error) {
	return 0, nil
}

// TestNextTaskID_UniqueUnderConcurrency reproduces the batch-collision bug : a
// turn dispatches task_create calls IN PARALLEL, so deriving the next id from
// the (write-behind) projection handed every task the same "t1". The atomic
// per-session counter must give every concurrent call a distinct id.
func TestNextTaskID_UniqueUnderConcurrency(t *testing.T) {
	e := &Engine{Sessions: taskIDFakeSessions{}}
	const n = 200
	ids := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) { defer wg.Done(); ids[i] = e.nextTaskID("sess-1") }(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate task id %q under concurrent task_create", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique ids, want %d", len(seen), n)
	}
	// A different session has its own counter, also starting at t1.
	if got := e.nextTaskID("sess-2"); got != "t1" {
		t.Fatalf("new session first id = %q, want t1", got)
	}
}
