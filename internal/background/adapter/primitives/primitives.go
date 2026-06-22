// Package primitives provides an adapter that bridges module-level events
// (published on the daemon's EventBus) into the background service's event
// pipeline. Each module that implements EventEmitter can push events that
// the background service processes through the same channel pipeline as
// webhooks, cron, or any other adapter.
//
// The adapter polls the daemon's GET /api/events/recent endpoint to receive
// events from the EventBus. This allows the background service (a separate
// process) to receive events without direct access to the daemon's in-process
// EventBus.
package primitives

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

// Adapter polls the daemon's /api/events/recent endpoint for events published
// on the EventBus and converts them into adapter.Event for the background
// service pipeline.
type Adapter struct {
	name      string
	daemonURL string
	sink      adapter.Sink
	logger    *slog.Logger
	cancel    context.CancelFunc
	client    *http.Client
	interval  time.Duration
	lastCheck time.Time
}

// New creates a new primitives adapter.
func New(daemonURL string, logger *slog.Logger) *Adapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{
		name:      "primitives",
		daemonURL: daemonURL,
		logger:    logger,
		client:    &http.Client{Timeout: 10 * time.Second},
		interval:  5 * time.Second,
	}
}

// Name returns the adapter type name.
func (a *Adapter) Name() string {
	return a.name
}

// Start polls the daemon's /api/events/recent endpoint and pushes events to the
// sink. Blocks until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	a.sink = sink
	ctx, a.cancel = context.WithCancel(ctx)

	a.logger.Info("primitives: adapter started",
		slog.String("daemon_url", a.daemonURL),
		slog.Duration("interval", a.interval))

	// Initial poll
	a.poll(ctx)

	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			a.poll(ctx)
		}
	}
}

// Send is a no-op for inbound-only adapters.
func (a *Adapter) Send(_ context.Context, _ map[string]any, _ string) error {
	return nil
}

// eventsResponse is the JSON shape returned by GET /api/events/recent.
type eventsResponse struct {
	Events []eventPayload `json:"events"`
	Count  int            `json:"count"`
}

// eventPayload is the JSON shape of an event from the daemon.
type eventPayload struct {
	Topic    string         `json:"topic"`
	Type     string         `json:"type"`
	Source   string         `json:"source,omitempty"`
	Data     any            `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// poll calls the daemon's /api/events/recent endpoint and converts events.
func (a *Adapter) poll(ctx context.Context) {
	url := fmt.Sprintf("%s/api/events/recent?since=%s", a.daemonURL, a.lastCheck.UTC().Format(time.RFC3339))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.logWarn("primitives: poll request build failed", err)
		return
	}

	resp, err := a.client.Do(req)
	if err != nil {
		a.logWarn("primitives: poll HTTP failed", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		a.logWarn("primitives: poll bad status", fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body)))
		return
	}

	var er eventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		a.logWarn("primitives: poll decode failed", err)
		return
	}

	a.lastCheck = time.Now().UTC()

	for i, evt := range er.Events {
		adpEvt := a.convertEvent(evt, i)
		if err := a.sink(ctx, adpEvt); err != nil {
			a.logWarn("primitives: sink failed", err)
		}
	}

	if len(er.Events) > 0 {
		a.logger.Info("primitives: events received", slog.Int("count", len(er.Events)))
	}
}

// convertEvent converts a daemon event payload to an adapter.Event.
func (a *Adapter) convertEvent(evt eventPayload, pos int) adapter.Event {
	dedupKey := a.buildDedupKey(evt, pos)

	payload := map[string]any{
		"topic": evt.Topic,
		"type":  evt.Type,
	}
	if evt.Source != "" {
		payload["source"] = evt.Source
	}
	if evt.Data != nil {
		payload["data"] = evt.Data
	}
	if len(evt.Metadata) > 0 {
		payload["metadata"] = evt.Metadata
	}

	return adapter.Event{
		Provider: "primitives", // Fixed provider name for routing
		Adapter:  a.name,
		DedupKey: dedupKey,
		Source:   evt.Source,
		Message:  evt.Type,
		Payload:  payload,
		Metadata: map[string]any{
			"topic":     evt.Topic,
			"type":      evt.Type,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
	}
}

// buildDedupKey creates a stable dedup key for an event.
func (a *Adapter) buildDedupKey(evt eventPayload, pos int) string {
	data := fmt.Sprintf("%s:%s:%s:%d", evt.Topic, evt.Type, evt.Source, pos)
	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("primitives:%x", hash[:8])
}

func (a *Adapter) logWarn(msg string, err error) {
	a.logger.Warn(msg, slog.String("err", err.Error()))
}

var _ adapter.Adapter = (*Adapter)(nil)
