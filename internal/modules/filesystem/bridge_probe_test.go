//go:build windows

package filesystem

import (
	"context"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

func TestProbe_BridgeRepo(t *testing.T) {
	root := `C:\Users\ASUS\Documents\digitorn-bridge.worktrees\copilot-worktree-2026-05-18T15-55-05`
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: t.TempDir()})
	ctx := workdir.WithPathPolicy(context.Background(), pp)

	dataOf := func(v any) (n, scanned int, trunc bool) {
		mp, _ := v.(map[string]any)
		if mp == nil {
			return
		}
		if ms, ok := mp["matches"].([]grepMatch); ok {
			n = len(ms)
		}
		if fs, ok := mp["files"].([]string); ok {
			n = len(fs)
		}
		if c, ok := mp["count"].(int); ok {
			n = c
		}
		if s, ok := mp["scanned"].(int); ok {
			scanned = s
		}
		trunc, _ = mp["truncated"].(bool)
		return
	}

	mem := func(tag string) {
		goruntime.GC()
		var ms goruntime.MemStats
		goruntime.ReadMemStats(&ms)
		t.Logf("MEM %-14s live-heap=%dMB sys=%dMB totalAlloc=%dMB numGC=%d",
			tag, ms.HeapAlloc>>20, ms.Sys>>20, ms.TotalAlloc>>20, ms.NumGC)
	}
	mem("start")

	st := time.Now()
	r, _ := m.glob(ctx, mustJSON(map[string]any{"pattern": "**/*.py", "tree": false}))
	n, _, _ := dataOf(r.Data)
	t.Logf("glob **/*.py      : %v  success=%v  %d files", time.Since(st), r.Success, n)

	for _, pat := range []string{"def ", "import ", "TODO|FIXME"} {
		st = time.Now()
		r, _ = m.grep(ctx, mustJSON(map[string]any{"pattern": pat}))
		matches, scanned, trunc := dataOf(r.Data)
		t.Logf("grep %-10q   : %v  success=%v  %d matches  scanned=%d  truncated=%v",
			pat, time.Since(st), r.Success, matches, scanned, trunc)
	}
	mem("after-grep")

	time.Sleep(2500 * time.Millisecond)
	st = time.Now()
	r, _ = m.grep(ctx, mustJSON(map[string]any{"pattern": "daemon"}))
	matches, scanned, _ := dataOf(r.Data)
	t.Logf("grep daemon (idx) : %v  success=%v  %d matches  scanned=%d", time.Since(st), r.Success, matches, scanned)
	mem("after-index")

	if gr, _ := m.glob(ctx, mustJSON(map[string]any{"pattern": "**/*.py", "tree": false})); gr.Success {
		if first := firstPy(gr.Data); first != "" {
			st = time.Now()
			rr, _ := m.read(ctx, mustJSON(map[string]any{"path": first, "outline": true}))
			body, _ := rr.Data.(string)
			t.Logf("read outline      : %v  success=%v  %d outline-bytes  (%s)", time.Since(st), rr.Success, len(body), first)
		}
	}
	mem("end")
}

func firstPy(v any) string {
	mp, _ := v.(map[string]any)
	if mp == nil {
		return ""
	}
	files, _ := mp["files"].([]string)
	for _, f := range files {
		if len(f) > 3 && f[len(f)-3:] == ".py" {
			return f
		}
	}
	if len(files) > 0 {
		return files[0]
	}
	return ""
}
