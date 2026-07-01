// Package poll is the shared engine for polling adapters (rss, imap, queues): a
// loop per provider that fetches new items since a durable cursor and emits them
// as Events. Correctness rests on two durable mechanisms: a per-item DedupKey
// (the intake drops re-seen items even across restarts) and a per-provider
// cursor committed after each round (resume-where-left-off, fixing the Python
// re-scan-from-scratch bug). The transport-specific part is just a Fetcher.
package poll

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

// Item is one fetched entry. ID is stable per item (guid / message-id / link)
// and becomes the DedupKey, so a redelivery is dropped.
type Item struct {
	ID      string
	Source  string
	Payload map[string]any
}

// Fetcher returns the items currently available, newest first, and the cursor
// representing the newest item. The poll loop decides which are new vs the
// stored cursor — the Fetcher itself is stateless.
type Fetcher interface {
	Fetch(ctx context.Context) (items []Item, newest string, err error)
}

// CursorStore persists a per-provider cursor (backed by the trigger row).
type CursorStore interface {
	Cursor(ctx context.Context, key string) string
	SetCursor(ctx context.Context, key, value string) error
}

// Provider is one armed poller.
type Provider struct {
	Name      string        // provider instance name (event.provider)
	Adapter   string        // adapter type (rss, imap, …)
	CursorKey string        // stable key for cursor storage (the trigger id)
	Interval  time.Duration // poll period
	Fetcher   Fetcher
}

// Run launches one loop per provider and blocks until ctx is cancelled.
func Run(ctx context.Context, providers []Provider, cursors CursorStore, sink adapter.Sink, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for _, p := range providers {
		go loop(ctx, p, cursors, sink, log)
	}
	<-ctx.Done()
}

func loop(ctx context.Context, p Provider, cursors CursorStore, sink adapter.Sink, log *slog.Logger) {
	if p.Interval <= 0 {
		p.Interval = 5 * time.Minute
	}
	pollOnce(ctx, p, cursors, sink, log) // immediate first poll
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pollOnce(ctx, p, cursors, sink, log)
		}
	}
}

// pollOnce runs a single fetch round: fetch → diff against the durable cursor →
// emit new items (newest-first, stop at the cursor) → advance the cursor. On the
// first arm (empty cursor) it records the newest without replaying history.
func pollOnce(ctx context.Context, p Provider, cursors CursorStore, sink adapter.Sink, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	items, newest, err := p.Fetcher.Fetch(ctx)
	if err != nil {
		log.Warn("background: poll fetch failed", "provider", p.Name, "err", err.Error())
		return
	}
	cur := cursors.Cursor(ctx, p.CursorKey)
	if cur == "" {
		if newest != "" {
			_ = cursors.SetCursor(ctx, p.CursorKey, newest)
		}
		return
	}
	for _, it := range items {
		if it.ID == cur {
			break // reached an already-seen item; the rest are older
		}
		if err := sink(ctx, adapter.Event{
			Provider: p.Name,
			Adapter:  p.Adapter,
			DedupKey: it.ID,
			Source:   it.Source,
			Payload:  it.Payload,
		}); err != nil {
			log.Warn("background: poll intake failed", "provider", p.Name, "err", err.Error())
		}
	}
	if newest != "" && newest != cur {
		_ = cursors.SetCursor(ctx, p.CursorKey, newest)
	}
}
