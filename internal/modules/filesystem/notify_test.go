package filesystem

import (
	"context"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

// capturingNotifier records every FileChanged signal for assertions.
type capturingNotifier struct {
	mu    sync.Mutex
	calls [][2]string // {sessionID, workdir}
}

func (n *capturingNotifier) FileChanged(sessionID, wd string, _ ...string) {
	n.mu.Lock()
	n.calls = append(n.calls, [2]string{sessionID, wd})
	n.mu.Unlock()
}

func (n *capturingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func (n *capturingNotifier) last() (string, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.calls) == 0 {
		return "", ""
	}
	c := n.calls[len(n.calls)-1]
	return c[0], c[1]
}

// agentCtx builds a dispatch ctx with the three values the agent path carries:
// a workdir PathPolicy, a caller identity, and (optionally) the live notifier.
func agentCtx(root, home, sid string, n tool.FileChangeNotifier) (context.Context, *workdir.PathPolicy) {
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: home})
	ctx := workdir.WithPathPolicy(context.Background(), pp)
	ctx = tool.WithIdentity(ctx, tool.Identity{SessionID: sid, AppID: "app", UserID: "u"})
	if n != nil {
		ctx = tool.WithFileChangeNotifier(ctx, n)
	}
	return ctx, &pp
}

// TestNotify_AllThreeMutatorsFire proves write / edit / multi_edit each emit
// exactly one live signal on success, carrying the caller's session id and the
// policy workdir root.
func TestNotify_AllThreeMutatorsFire(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	n := &capturingNotifier{}
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": t.TempDir()}); err != nil {
		t.Fatalf("init: %v", err)
	}
	ctx, pp := agentCtx(root, home, "sessX", n)
	wantRoot := pp.Root()

	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "a.txt", "content": "x\n"})); err != nil || !r.Success {
		t.Fatalf("write: err=%v res=%v", err, r.Error)
	}
	if n.count() != 1 {
		t.Fatalf("write must fire 1 notify, got %d", n.count())
	}
	if r, err := m.edit(ctx, mustJSON(map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y"})); err != nil || !r.Success {
		t.Fatalf("edit: err=%v res=%v", err, r.Error)
	}
	if n.count() != 2 {
		t.Fatalf("edit must fire 1 notify (total 2), got %d", n.count())
	}
	if r, err := m.multiEdit(ctx, mustJSON(map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"old_string": "y", "new_string": "z"}},
	})); err != nil || !r.Success {
		t.Fatalf("multi_edit: err=%v res=%v", err, r.Error)
	}
	if n.count() != 3 {
		t.Fatalf("multi_edit must fire 1 notify (total 3), got %d", n.count())
	}
	sid, wd := n.last()
	if sid != "sessX" || wd != wantRoot {
		t.Fatalf("notify carried (%q,%q), want (sessX,%q)", sid, wd, wantRoot)
	}
}

// TestNotify_DryRunDoesNotFire proves a previewed edit/multi_edit (dry_run) never
// pushes a live change — only a real write does.
func TestNotify_DryRunDoesNotFire(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	n := &capturingNotifier{}
	m := New()
	_ = m.Init(context.Background(), map[string]any{"workspace": t.TempDir()})
	ctx, _ := agentCtx(root, home, "s", n)

	// Seed a file with a real write (1 notify).
	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "a.txt", "content": "hello\n"})); err != nil || !r.Success {
		t.Fatalf("seed write: %v %v", err, r.Error)
	}
	base := n.count()

	if r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "a.txt", "old_string": "hello", "new_string": "world", "dry_run": true,
	})); err != nil || !r.Success {
		t.Fatalf("dry edit: %v %v", err, r.Error)
	}
	if r, err := m.multiEdit(ctx, mustJSON(map[string]any{
		"path": "a.txt", "dry_run": true,
		"edits": []map[string]any{{"old_string": "hello", "new_string": "world"}},
	})); err != nil || !r.Success {
		t.Fatalf("dry multi: %v %v", err, r.Error)
	}
	if n.count() != base {
		t.Fatalf("dry_run must not fire a live push: count went %d -> %d", base, n.count())
	}
}

// TestNotify_GatedOnContext proves the signal fires ONLY when a notifier, a
// caller identity AND a workdir policy all ride on ctx — the non-agent paths
// (setup / CLI / admin) never push.
func TestNotify_GatedOnContext(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	m := New()
	_ = m.Init(context.Background(), map[string]any{"workspace": root})

	// (a) no notifier on ctx → no panic, no call (nothing to assert beyond success).
	ctxNoNotif, _ := agentCtx(root, home, "s", nil)
	if r, err := m.write(ctxNoNotif, mustJSON(map[string]any{"path": "a.txt", "content": "1"})); err != nil || !r.Success {
		t.Fatalf("write without notifier must still succeed: %v %v", err, r.Error)
	}

	// (b) notifier + identity but NO PathPolicy → gated off.
	n := &capturingNotifier{}
	ctxNoPolicy := tool.WithIdentity(context.Background(), tool.Identity{SessionID: "s"})
	ctxNoPolicy = tool.WithFileChangeNotifier(ctxNoPolicy, n)
	notifyFileChange(ctxNoPolicy)
	if n.count() != 0 {
		t.Fatalf("no PathPolicy must gate the push off, got %d", n.count())
	}

	// (c) notifier + policy but NO identity → gated off.
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: home})
	ctxNoID := workdir.WithPathPolicy(context.Background(), pp)
	ctxNoID = tool.WithFileChangeNotifier(ctxNoID, n)
	notifyFileChange(ctxNoID)
	if n.count() != 0 {
		t.Fatalf("no identity must gate the push off, got %d", n.count())
	}

	// (d) all three present → fires once.
	ctxAll, _ := agentCtx(root, home, "s", n)
	notifyFileChange(ctxAll)
	if n.count() != 1 {
		t.Fatalf("all three present must fire once, got %d", n.count())
	}
}
