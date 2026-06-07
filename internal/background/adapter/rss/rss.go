// Package rss is the RSS/Atom polling adapter: it fetches a feed on an interval
// and emits an Event per new entry (new = not seen since the durable cursor).
// It parses both RSS 2.0 and Atom with the standard library — no dependency.
package rss

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
	"github.com/mbathepaul/digitorn/internal/background/adapter/poll"
)

const (
	maxFeedBytes = 8 << 20 // 8 MB
	maxItems     = 200     // bound a hostile/huge feed
)

// Provider is one armed feed.
type Provider struct {
	Name      string
	URL       string
	CursorKey string
	Interval  time.Duration
}

// Adapter polls a set of feeds. Inbound-only (Send is a no-op).
type Adapter struct {
	providers []poll.Provider
	cursors   poll.CursorStore
	log       *slog.Logger
}

// New builds the adapter over the feeds, sharing one HTTP client.
func New(providers []Provider, cursors poll.CursorStore, log *slog.Logger) *Adapter {
	hc := &http.Client{Timeout: 20 * time.Second}
	pp := make([]poll.Provider, 0, len(providers))
	for _, p := range providers {
		pp = append(pp, poll.Provider{
			Name:      p.Name,
			Adapter:   "rss",
			CursorKey: p.CursorKey,
			Interval:  p.Interval,
			Fetcher:   &feedFetcher{url: p.URL, hc: hc},
		})
	}
	return &Adapter{providers: pp, cursors: cursors, log: log}
}

func (a *Adapter) Name() string { return "rss" }

// Send is a no-op: rss is inbound-only.
func (a *Adapter) Send(context.Context, map[string]any, string) error { return nil }

// Start runs the poll loops until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	poll.Run(ctx, a.providers, a.cursors, sink, a.log)
	return nil
}

// feedFetcher fetches + parses one feed.
type feedFetcher struct {
	url string
	hc  *http.Client
}

func (f *feedFetcher) Fetch(ctx context.Context) ([]poll.Item, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml")
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("rss: %s returned %d", f.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, "", err
	}
	return parseFeed(body)
}

// ── parsing (RSS 2.0 + Atom in one tolerant shape) ──────────────────────────

type xmlFeed struct {
	Channel struct {
		Title string    `xml:"title"`
		Items []xmlItem `xml:"item"`
	} `xml:"channel"`
	Title   string    `xml:"title"` // atom feed title
	Entries []xmlItem `xml:"entry"` // atom
}

type xmlItem struct {
	GUID      string    `xml:"guid"`
	ID        string    `xml:"id"` // atom
	Title     string    `xml:"title"`
	Links     []xmlLink `xml:"link"`
	Desc      string    `xml:"description"`
	Summary   string    `xml:"summary"`
	PubDate   string    `xml:"pubDate"`
	Updated   string    `xml:"updated"`
	Published string    `xml:"published"`
}

type xmlLink struct {
	Href string `xml:"href,attr"`
	Text string `xml:",chardata"`
}

func parseFeed(body []byte) ([]poll.Item, string, error) {
	var f xmlFeed
	if err := xml.Unmarshal(body, &f); err != nil {
		return nil, "", fmt.Errorf("rss: parse: %w", err)
	}
	raw := f.Channel.Items
	if len(raw) == 0 {
		raw = f.Entries
	}
	if len(raw) > maxItems {
		raw = raw[:maxItems]
	}
	items := make([]poll.Item, 0, len(raw))
	for _, it := range raw {
		link := itemLink(it.Links)
		id := firstNonEmpty(strings.TrimSpace(it.GUID), strings.TrimSpace(it.ID), link, strings.TrimSpace(it.Title))
		if id == "" {
			continue
		}
		items = append(items, poll.Item{
			ID:     id,
			Source: link,
			Payload: map[string]any{
				"title":     strings.TrimSpace(it.Title),
				"link":      link,
				"summary":   firstNonEmpty(strings.TrimSpace(it.Desc), strings.TrimSpace(it.Summary)),
				"published": firstNonEmpty(strings.TrimSpace(it.PubDate), strings.TrimSpace(it.Published), strings.TrimSpace(it.Updated)),
				"id":        id,
			},
		})
	}
	newest := ""
	if len(items) > 0 {
		newest = items[0].ID
	}
	return items, newest, nil
}

func itemLink(ls []xmlLink) string {
	for _, l := range ls {
		if h := strings.TrimSpace(l.Href); h != "" {
			return h
		}
		if t := strings.TrimSpace(l.Text); t != "" {
			return t
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
