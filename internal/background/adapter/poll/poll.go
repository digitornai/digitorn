package poll

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

type Item struct {
	ID      string
	Source  string
	Payload map[string]any
}

type Fetcher interface {
	Fetch(ctx context.Context) (items []Item, newest string, err error)
}

type CursorStore interface {
	Cursor(ctx context.Context, key string) string
	SetCursor(ctx context.Context, key, value string) error
}

type Provider struct {
	Name      string
	Adapter   string
	CursorKey string
	Interval  time.Duration
	Fetcher   Fetcher
}

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
	pollOnce(ctx, p, cursors, sink, log)
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
			break
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
