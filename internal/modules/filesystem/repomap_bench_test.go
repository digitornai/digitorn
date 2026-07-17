//go:build treesitter

package filesystem

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/context/repomap"
)

func TestRepomapTiming(t *testing.T) {
	root := "/home/paul/codes/digitorn"
	if _, err := os.Stat(root); err != nil {
		t.Skip("digitorn root not found")
	}

	fmt.Printf("\n=== Repomap timing (root: %s, CPUs: %d) ===\n", root, runtime.NumCPU())

	t0 := time.Now()
	entries := walkRepo(root)
	walkDur := time.Since(t0)
	fmt.Printf("Walk (cold)    : %-10v  %d files\n", walkDur, len(entries))

	t1 := time.Now()
	var totalSyms int
	cache := make(map[string]repomap.FileSyms, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(e.Abs)
		if err != nil {
			continue
		}
		fs, ok := parseOneFile(e.Rel, b)
		if !ok {
			continue
		}
		cache[e.Rel] = fs
		totalSyms += len(fs.Syms)
	}
	parseDur := time.Since(t1)
	fmt.Printf("Parse (cold)   : %-10v  %d syms  (%d files parsed)\n", parseDur, totalSyms, len(cache))

	t2 := time.Now()
	g := repomap.Graph{Calls: make(map[string][]string)}
	for _, fs := range cache {
		g.Syms = append(g.Syms, fs.Syms...)
		for k, calls := range fs.Calls {
			g.Calls[k] = calls
		}
	}
	rendered := repomap.Render(g, 8000)
	rankDur := time.Since(t2)
	fmt.Printf("Rank+Render    : %-10v  %d chars output\n", rankDur, len(rendered))
	fmt.Printf("TOTAL (cold)   : %v\n\n", time.Since(t0))

	var oneFile repomap.WalkEntry
	for _, e := range entries {
		oneFile = e
		break
	}
	delete(cache, oneFile.Rel)

	t3 := time.Now()
	entries2 := walkRepo(root)
	walkDur2 := time.Since(t3)

	t4 := time.Now()
	reparsed := 0
	for _, e := range entries2 {
		if _, cached := cache[e.Rel]; !cached {
			b, err := os.ReadFile(e.Abs)
			if err != nil {
				continue
			}
			fs, ok := parseOneFile(e.Rel, b)
			if ok {
				cache[e.Rel] = fs
				reparsed++
			}
		}
	}
	parseDur2 := time.Since(t4)

	t5 := time.Now()
	g2 := repomap.Graph{Calls: make(map[string][]string)}
	for _, fs := range cache {
		g2.Syms = append(g2.Syms, fs.Syms...)
		for k, calls := range fs.Calls {
			g2.Calls[k] = calls
		}
	}
	repomap.Render(g2, 8000)
	rankDur2 := time.Since(t5)

	fmt.Printf("Walk (warm)    : %-10v\n", walkDur2)
	fmt.Printf("Parse (warm)   : %-10v  %d file re-parsed\n", parseDur2, reparsed)
	fmt.Printf("Rank+Render    : %-10v\n", rankDur2)
	fmt.Printf("TOTAL (warm)   : %v\n", time.Since(t3))

	if len(rendered) > 800 {
		fmt.Printf("\n--- Rendered snippet (first 800 chars) ---\n%s\n---\n", rendered[:800])
	} else {
		fmt.Printf("\n--- Rendered output ---\n%s\n---\n", rendered)
	}
}
