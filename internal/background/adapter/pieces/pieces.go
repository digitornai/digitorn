// Package pieces is the Activepieces polling trigger adapter. Each armed
// trigger calls /trigger/poll on the bridge trigger server at its configured
// interval, persists the storeState (Activepieces cursor) between polls, and
// emits one Event per returned event via the shared Sink.
package pieces

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

// CursorStore persists the Activepieces storeState between polls.
type CursorStore interface {
	Cursor(ctx context.Context, key string) string
	SetCursor(ctx context.Context, key, value string) error
}

// Provider is one armed Pieces trigger.
type Provider struct {
	Name       string
	TriggerURL string
	Piece      string
	Trigger    string
	Auth       any
	Props      map[string]any
	CursorKey  string
	Interval   time.Duration
}

// Adapter polls a set of Pieces triggers with their own storeState-aware loop.
type Adapter struct {
	mu        sync.Mutex
	providers []Provider
	cursors   CursorStore
	log       *slog.Logger
	client    *http.Client
	ctx       context.Context
	sink      adapter.Sink
}

func New(providers []Provider, cursors CursorStore, log *slog.Logger) *Adapter {
	return &Adapter{
		providers: providers,
		cursors:   cursors,
		log:       log,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Adapter) Name() string                                       { return "pieces" }
func (a *Adapter) Send(context.Context, map[string]any, string) error { return nil }

func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	a.mu.Lock()
	a.ctx = ctx
	a.sink = sink
	for _, p := range a.providers {
		go a.loop(ctx, p, sink)
	}
	a.mu.Unlock()
	<-ctx.Done()
	return nil
}

// Arm adds a provider and starts polling it live without restarting the service.
func (a *Adapter) Arm(p Provider) {
	a.mu.Lock()
	ctx, sink := a.ctx, a.sink
	a.providers = append(a.providers, p)
	a.mu.Unlock()
	if ctx != nil && sink != nil {
		go a.loop(ctx, p, sink)
	}
}

func (a *Adapter) loop(ctx context.Context, p Provider, sink adapter.Sink) {
	interval := p.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	a.poll(ctx, p, sink) // immediate first poll
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.poll(ctx, p, sink)
		}
	}
}

type pollRequest struct {
	Piece      string         `json:"piece"`
	Trigger    string         `json:"trigger"`
	Auth       any            `json:"auth"`
	Props      map[string]any `json:"props"`
	StoreState map[string]any `json:"storeState"`
}

type pollResponse struct {
	Events     []map[string]any `json:"events"`
	StoreState map[string]any   `json:"storeState"`
}

func (a *Adapter) poll(ctx context.Context, p Provider, sink adapter.Sink) {
	// Load persisted storeState from cursor store.
	storeState := map[string]any{}
	if cur := a.cursors.Cursor(ctx, p.CursorKey); cur != "" {
		_ = json.Unmarshal([]byte(cur), &storeState)
	}
	firstRun := len(storeState) == 0

	body, _ := json.Marshal(pollRequest{
		Piece:      p.Piece,
		Trigger:    p.Trigger,
		Auth:       p.Auth,
		Props:      p.Props,
		StoreState: storeState,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.TriggerURL+"/trigger/poll", bytes.NewReader(body))
	if err != nil {
		a.log.Warn("pieces: poll request build failed", "provider", p.Name, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Warn("pieces: poll HTTP failed", "provider", p.Name, "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		a.log.Warn("pieces: poll bad status", "provider", p.Name, "status", resp.StatusCode, "body", string(body))
		return
	}

	var pr pollResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		a.log.Warn("pieces: poll decode failed", "provider", p.Name, "err", err)
		return
	}

	// Persist new storeState (cursor) regardless of events.
	if len(pr.StoreState) > 0 {
		if data, err := json.Marshal(pr.StoreState); err == nil {
			_ = a.cursors.SetCursor(ctx, p.CursorKey, string(data))
		}
	}

	// On first run, initialize cursor but don't emit events (avoid replay).
	if firstRun {
		return
	}

	for i, ev := range pr.Events {
		normalized := normalizePiecesPayload(ev, p.Piece, p.Trigger)
		if err := sink(ctx, adapter.Event{
			Provider: p.Name,
			Adapter:  "pieces",
			DedupKey: eventID(normalized, p.Piece, p.Trigger, i),
			Source:   p.Piece + "." + p.Trigger,
			Message:  extractMessage(normalized),
			Payload:  normalized,
		}); err != nil {
			a.log.Warn("pieces: sink failed", "provider", p.Name, "err", err)
		}
	}

	if len(pr.Events) > 0 {
		a.log.Info("pieces: trigger fired", "provider", p.Name,
			"piece", p.Piece, "trigger", p.Trigger, "events", len(pr.Events))
	}
}

func normalizePiecesPayload(raw map[string]any, piece, trigger string) map[string]any {
	out := make(map[string]any, len(raw)+2)
	for k, v := range raw {
		out[k] = v
	}
	out["piece"] = piece
	out["trigger"] = trigger

	commonFields := []string{"subject", "title", "from", "to", "body", "snippet",
		"content", "text", "name", "url", "id", "email", "date", "summary"}
	for _, k := range []string{"message", "page", "item", "record", "data", "event", "object"} {
		if nested, ok := raw[k].(map[string]any); ok {
			for _, field := range commonFields {
				if _, exists := out[field]; !exists {
					if v, ok := nested[field]; ok {
						out[field] = v
					}
				}
			}
		}
	}
	return out
}

func extractMessage(payload map[string]any) string {
	for _, k := range []string{"subject", "title", "name", "text", "content", "snippet", "summary", "body"} {
		if v, ok := payload[k].(string); ok && v != "" {
			if len(v) > 200 {
				v = v[:200] + "..."
			}
			return v
		}
	}
	return ""
}

func eventID(ev map[string]any, piece, trigger string, pos int) string {
	for _, k := range []string{"id", "messageId", "emailId", "eventId", "uid", "guid"} {
		if v, ok := ev[k].(string); ok && v != "" {
			return piece + ":" + trigger + ":" + v
		}
	}
	b, _ := json.Marshal(ev)
	h := fmt.Sprintf("%x", b)
	if len(h) > 16 {
		h = h[:16]
	}
	return fmt.Sprintf("%s:%s:pos%d:%s", piece, trigger, pos, h)
}

// TriggerURL returns the default bridge trigger server URL for the given port.
func TriggerURL(port int) string {
	return strings.TrimRight(fmt.Sprintf("http://127.0.0.1:%d", port), "/")
}
