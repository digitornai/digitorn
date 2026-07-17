package poll

import (
	"context"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

type fakeFetcher struct {
	mu    sync.Mutex
	items []Item
}

func (f *fakeFetcher) set(items ...Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
}

func (f *fakeFetcher) Fetch(context.Context) ([]Item, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	newest := ""
	if len(f.items) > 0 {
		newest = f.items[0].ID
	}
	return append([]Item(nil), f.items...), newest, nil
}

type memCursors struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *memCursors) Cursor(_ context.Context, k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}
func (c *memCursors) SetCursor(_ context.Context, k, v string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
	return nil
}

func item(id string) Item { return Item{ID: id, Payload: map[string]any{"id": id}} }

func TestPollOnce_CursorLifecycle(t *testing.T) {
	ctx := context.Background()
	f := &fakeFetcher{}
	f.set(item("g3"), item("g2"), item("g1"))
	cur := &memCursors{m: map[string]string{}}
	p := Provider{Name: "feed", Adapter: "rss", CursorKey: "k", Fetcher: f}

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	pollOnce(ctx, p, cur, sink, nil)
	if len(got) != 0 {
		t.Fatalf("first arm replayed history: %d", len(got))
	}
	if cur.Cursor(ctx, "k") != "g3" {
		t.Fatalf("cursor not set to newest, got %q", cur.Cursor(ctx, "k"))
	}

	f.set(item("g4"), item("g3"), item("g2"))
	pollOnce(ctx, p, cur, sink, nil)
	if len(got) != 1 || got[0].DedupKey != "g4" {
		t.Fatalf("should emit only g4, got %+v", got)
	}
	if cur.Cursor(ctx, "k") != "g4" {
		t.Fatalf("cursor should advance to g4, got %q", cur.Cursor(ctx, "k"))
	}

	got = nil
	pollOnce(ctx, p, cur, sink, nil)
	if len(got) != 0 {
		t.Fatalf("repeat poll emitted %d", len(got))
	}

	got = nil
	f.set(item("g6"), item("g5"), item("g4"))
	pollOnce(ctx, p, cur, sink, nil)
	if len(got) != 2 || got[0].DedupKey != "g6" || got[1].DedupKey != "g5" {
		t.Fatalf("should emit g6,g5, got %+v", got)
	}
}
