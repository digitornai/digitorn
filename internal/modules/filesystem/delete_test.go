package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDelete_RemovesFileAndNotifies(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	n := &capturingNotifier{}
	m := New()
	_ = m.Init(context.Background(), map[string]any{"workspace": root})
	ctx, _ := agentCtx(root, home, "s", n)

	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "a.txt", "content": "x"})); err != nil || !r.Success {
		t.Fatalf("seed write: %v %v", err, r.Error)
	}
	base := n.count()

	r, err := m.delete(ctx, mustJSON(map[string]any{"path": "a.txt"}))
	if err != nil || !r.Success {
		t.Fatalf("delete: %v %v", err, r.Error)
	}
	if _, e := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(e) {
		t.Fatal("file must be gone after delete")
	}
	if n.count() != base+1 {
		t.Fatalf("delete must fire one live push, got %d (base %d)", n.count(), base)
	}
}

func TestDelete_MissingErrors(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	m := New()
	_ = m.Init(context.Background(), map[string]any{"workspace": root})
	ctx, _ := agentCtx(root, home, "s", nil)
	if r, _ := m.delete(ctx, mustJSON(map[string]any{"path": "nope.txt"})); r.Success {
		t.Fatal("delete of a missing file must error (no silent no-op)")
	}
}

func TestDelete_DirectoryRefused(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := New()
	_ = m.Init(context.Background(), map[string]any{"workspace": root})
	ctx, _ := agentCtx(root, home, "s", nil)
	if r, _ := m.delete(ctx, mustJSON(map[string]any{"path": "sub"})); r.Success {
		t.Fatal("delete of a directory must be refused")
	}
	if _, e := os.Stat(filepath.Join(root, "sub")); e != nil {
		t.Fatal("directory must remain after a refused delete")
	}
}

func TestDelete_EscapeRejected(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	m := New()
	_ = m.Init(context.Background(), map[string]any{"workspace": root})
	ctx, _ := agentCtx(root, home, "s", nil)
	if r, _ := m.delete(ctx, mustJSON(map[string]any{"path": "../../../etc/passwd"})); r.Success {
		t.Fatal("an escaping delete must be rejected by the path policy")
	}
}
