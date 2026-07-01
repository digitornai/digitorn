package tui

import (
	"testing"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

// The per-message cache must be transparent : a warm rebuild produces the
// exact same scrollback as a cold one, and the cache is pruned to the messages
// currently on screen (no unbounded growth).
func TestRenderCache_TransparentAndPruned(t *testing.T) {
	m := NewMessages(theme.Default())
	m.SetSize(80, 24)
	msgs := []client.Message{
		{Role: "user", Content: "hi", Seq: 1},
		{Role: "assistant", Content: "**hello** world", Seq: 2},
		{Role: "tool", Content: "filesystem.read", CallID: "c1", Status: "completed", DurationMs: 5, Seq: 3},
	}
	m.SetMessages(msgs)
	warm := m.committed
	if len(m.renderCache) != 3 {
		t.Fatalf("cache size = %d, want 3 (one per cacheable message)", len(m.renderCache))
	}

	// A cold rebuild (cache dropped) must yield byte-identical output.
	m.Rebuild()
	if m.committed != warm {
		t.Fatal("caching changed the rendered scrollback (must be transparent)")
	}

	// Growing the transcript prunes the cache to exactly the live messages.
	msgs = append(msgs, client.Message{Role: "user", Content: "again", Seq: 4})
	m.SetMessages(msgs)
	if len(m.renderCache) != 4 {
		t.Fatalf("cache size = %d, want 4 after append (pruned to live set)", len(m.renderCache))
	}
}

func TestBlockCacheKey(t *testing.T) {
	base := client.Message{Role: "assistant", Content: "x"}
	k1, ok := blockCacheKey(base, 80, false, false)
	if !ok {
		t.Fatal("a committed assistant message must be cacheable")
	}
	if k2, _ := blockCacheKey(base, 80, false, false); k2 != k1 {
		t.Fatal("identical inputs must yield the same key")
	}
	// Any render-affecting change must change the key.
	mut := []client.Message{
		{Role: "assistant", Content: "y"},
		{Role: "user", Content: "x"},
	}
	for _, mm := range mut {
		if k, _ := blockCacheKey(mm, 80, false, false); k == k1 {
			t.Fatalf("changed message %+v must change the key", mm)
		}
	}
	if k, _ := blockCacheKey(base, 40, false, false); k == k1 {
		t.Fatal("width must be part of the key")
	}
	if k, _ := blockCacheKey(base, 80, true, false); k == k1 {
		t.Fatal("selection must be part of the key")
	}

	// A running chip is never cached (its duration is live).
	if _, ok := blockCacheKey(client.Message{Role: "tool", Status: "running", CallID: "c"}, 80, false, false); ok {
		t.Fatal("a running tool chip must NOT be cacheable")
	}
	// A finished chip is cacheable, and diff/expanded state are part of the key.
	done := client.Message{Role: "tool", Status: "completed", CallID: "c"}
	kc, ok := blockCacheKey(done, 80, false, false)
	if !ok {
		t.Fatal("a completed tool chip must be cacheable")
	}
	withDiff := done
	withDiff.ToolDiff = "+1 -0"
	if k, _ := blockCacheKey(withDiff, 80, false, false); k == kc {
		t.Fatal("ToolDiff must be part of the key")
	}
	if k, _ := blockCacheKey(done, 80, false, true); k == kc {
		t.Fatal("expanded state must be part of the key")
	}
}
