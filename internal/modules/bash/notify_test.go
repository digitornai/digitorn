package bash

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

type capturingNotifier struct {
	mu sync.Mutex
	n  int
}

func (c *capturingNotifier) FileChanged(string, string) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *capturingNotifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *capturingNotifier) reset() {
	c.mu.Lock()
	c.n = 0
	c.mu.Unlock()
}

// EVERY shell command pokes the live workspace notifier so the web Changes panel
// and file tree refresh — exactly like a filesystem.write. We never gate on the
// mtime-based files-changed scan: it misses mv/rename (which preserve mtime), the
// very pattern a scaffolder uses ("create in a sub-dir, then mv to the root"). The
// shadow-repo git status, recomputed on the debounced poke, is the arbiter.
func TestRun_ShellAlwaysNotifiesWorkspace(t *testing.T) {
	m := newGoShellModule(t)
	n := &capturingNotifier{}
	pp := workdir.NewPolicy(workdir.Options{Root: m.cfg.Workdir})
	ctx := workdir.WithPathPolicy(context.Background(), pp)
	ctx = tool.WithIdentity(ctx, tool.Identity{SessionID: "s1"})
	ctx = tool.WithFileChangeNotifier(ctx, n)

	run := func(cmd string) {
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		if _, err := m.run(ctx, raw); err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
	}

	run(`echo hi > made.txt`)
	if n.count() < 1 {
		t.Fatal("a file-creating shell command must notify the workspace")
	}
	// A command whose mtime scan would find nothing still pokes — git status decides.
	before := n.count()
	run(`echo just-reading`)
	if n.count() <= before {
		t.Fatalf("every command must notify (robust to mv/rename), count %d -> %d", before, n.count())
	}
}

// Without a notifier/identity/policy on ctx (setup / CLI / test calls), a write
// must run cleanly and never panic — the poke is best-effort and skips silently.
func TestRun_NoNotifierOnContextIsSafe(t *testing.T) {
	m := newGoShellModule(t)
	raw, _ := json.Marshal(map[string]any{"command": `echo hi > made.txt`})
	if _, err := m.run(context.Background(), raw); err != nil {
		t.Fatalf("run without notifier: %v", err)
	}
}
