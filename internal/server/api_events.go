package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mbathepaul/digitorn/internal/ports"
)

const (
	// maxEventBufferSize is the maximum number of events stored in the buffer.
	// When full, the oldest event is evicted. This prevents unbounded memory
	// growth while keeping enough history for the background service to poll.
	maxEventBufferSize = 100

	// maxRecentWindow is the maximum time window for "recent" events.
	// Prevents the background service from fetching a huge batch if it was
	// down for a long time.
	maxRecentWindow = 5 * time.Minute
)

// eventBuffer is a circular buffer that stores recent events from the EventBus.
// It is used by the /api/events/recent endpoint so the background service can
// poll for new events. SAFETY: Never blocks callers; uses non-blocking lock
// acquisition where possible.
type eventBuffer struct {
	mu      sync.RWMutex
	events  []ports.Event
	maxSize int

	// Metrics (lock-free)
	added   atomic.Uint64
	evicted atomic.Uint64
	fetched atomic.Uint64
}

var globalEventBuffer = &eventBuffer{
	events:  make([]ports.Event, 0, maxEventBufferSize),
	maxSize: maxEventBufferSize,
}

// addEvent appends an event to the buffer, evicting the oldest if full.
// NON-BLOCKING: if the lock is contended, the event is dropped.
func (b *eventBuffer) addEvent(evt ports.Event) {
	// Non-blocking lock acquisition: if we can't get the lock immediately,
	// drop the event rather than block the publisher.
	if !b.mu.TryLock() {
		b.evicted.Add(1) // counted as dropped
		return
	}
	defer b.mu.Unlock()

	if len(b.events) >= b.maxSize {
		b.events = b.events[1:]
		b.evicted.Add(1)
	}
	b.events = append(b.events, evt)
	b.added.Add(1)
}

// recentEvents returns events published after the given timestamp.
// Capped to maxRecentWindow to prevent huge responses.
func (b *eventBuffer) recentEvents(since time.Time) []ports.Event {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Cap the window to prevent huge responses
	cutoff := time.Now().UTC().Add(-maxRecentWindow)
	if since.Before(cutoff) {
		since = cutoff
	}

	var result []ports.Event
	for _, evt := range b.events {
		if ts, ok := evt.Metadata["timestamp"].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(since) {
				result = append(result, evt)
			}
		}
	}
	b.fetched.Add(1)
	return result
}

// subscribeToEventBus starts a goroutine that forwards all EventBus events to
// the eventBuffer. Called during daemon bootstrap.
func (d *Daemon) subscribeToEventBus() {
	if d.eventBus == nil {
		return
	}
	// Subscribe to all events using a wildcard topic ("")
	_, _ = d.eventBus.Subscribe("", func(ctx context.Context, evt ports.Event) error {
		// Add timestamp if not present
		if evt.Metadata == nil {
			evt.Metadata = make(map[string]any)
		}
		if _, ok := evt.Metadata["timestamp"]; !ok {
			evt.Metadata["timestamp"] = time.Now().UTC().Format(time.RFC3339)
		}
		globalEventBuffer.addEvent(evt)
		return nil
	})
}

// eventsResponse is the JSON shape returned by GET /api/events/recent.
type eventsResponse struct {
	Events []ports.Event `json:"events"`
	Count  int           `json:"count"`
}

// handleEventsRecent returns events published since the ?since query parameter.
// Used by the background service's primitives adapter to poll for new events.
func (d *Daemon) handleEventsRecent(w http.ResponseWriter, r *http.Request) {
	sinceStr := r.URL.Query().Get("since")
	var since time.Time
	if sinceStr != "" {
		var err error
		since, err = time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			http.Error(w, "invalid 'since' parameter (use RFC3339)", http.StatusBadRequest)
			return
		}
	} else {
		// Default: last 60 seconds
		since = time.Now().UTC().Add(-60 * time.Second)
	}

	events := globalEventBuffer.recentEvents(since)
	if events == nil {
		events = []ports.Event{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(eventsResponse{
		Events: events,
		Count:  len(events),
	})
}
