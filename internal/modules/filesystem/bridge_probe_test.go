//go:build windows

package filesystem

import (
	"context"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

// TestProbe_BridgeRepo drives the filesystem tools against a real ~4k-file repo
// and reports timing + memory, to prove glob/grep/read-outline + the trigram
// index are fast and BOUNDED. The memory question that matters: is the heap a
// LIVE retention (a leak / unbounded structure → OOM risk on bigger trees) or
// just uncollected churn (GC pacing, reclaimed on demand → harmless)? We answer
// it by forcing a GC before every reading: post-GC heapAlloc is LIVE memory.
func TestProbe_BridgeRepo(t *testing.T) {
	root := `C:\Users\ASUS\Documents\digitorn-bridge.worktrees\copilot-worktree-2026-05-18T15-55-05`
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: t.TempDir()})
	ctx := workdir.WithPathPolicy(context.Background(), pp)

	// dataOf reads the real result shape: Data is map[string]any with
	// "matches" / "files" / "count" + "scanned" + "truncated".
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
		goruntime.GC() // force a collection so heapAlloc reflects LIVE memory, not garbage
		var ms goruntime.MemStats
		goruntime.ReadMemStats(&ms)
		t.Logf("MEM %-14s live-heap=%dMB sys=%dMB totalAlloc=%dMB numGC=%d",
			tag, ms.HeapAlloc>>20, ms.Sys>>20, ms.TotalAlloc>>20, ms.NumGC)
	}
	mem("start")

	// glob — count results from files[] not a string body.
	st := time.Now()
	r, _ := m.glob(ctx, mustJSON(map[string]any{"pattern": "**/*.py", "tree": false}))
	n, _, _ := dataOf(r.Data)
	t.Logf("glob **/*.py      : %v  success=%v  %d files", time.Since(st), r.Success, n)

	// grep (regex over the whole repo — exercises trigram index build + scan).
	for _, pat := range []string{"def ", "import ", "TODO|FIXME"} {
		st = time.Now()
		r, _ = m.grep(ctx, mustJSON(map[string]any{"pattern": pat}))
		matches, scanned, trunc := dataOf(r.Data)
		t.Logf("grep %-10q   : %v  success=%v  %d matches  scanned=%d  truncated=%v",
			pat, time.Since(st), r.Success, matches, scanned, trunc)
	}
	mem("after-grep")

	// Let the async trigram index build settle, then grep again (index path).
	time.Sleep(2500 * time.Millisecond)
	st = time.Now()
	r, _ = m.grep(ctx, mustJSON(map[string]any{"pattern": "daemon"}))
	matches, scanned, _ := dataOf(r.Data)
	t.Logf("grep daemon (idx) : %v  success=%v  %d matches  scanned=%d", time.Since(st), r.Success, matches, scanned)
	mem("after-index")

	// read with outline on a real Python file (navigate a big file cheaply).
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

// firstPy returns the first .py path from a glob result map.
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
