// Package eventbus provides the in-memory implementation of ports.EventBus.
// It is a lightweight pub/sub bus for intra-process event delivery between
// modules and the runtime. Delivery is async (worker pool) and best-effort:
// handler errors are logged but do not block other subscribers.
//
// SAFETY: The bus NEVER blocks the caller. Publish is non-blocking and drops
// events when the internal channel is full (backpressure). The worker pool
// has a fixed number of goroutines to prevent saturation.
package eventbus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/mbathepaul/digitorn/internal/ports"
)

var ErrBusClosed = errors.New("eventbus: bus is closed")
var ErrBusFull = errors.New("eventbus: channel full, event dropped")

const (
	// defaultWorkerCount is the number of goroutines processing events.
	// Too few = events pile up; too many = CPU saturation. 4 is safe for
	// typical module event rates (< 100 events/sec).
	defaultWorkerCount = 4

	// defaultChannelSize is the buffer between Publish and handlers.
	// When full, Publish drops the event (non-blocking). This prevents
	// the caller from ever blocking.
	defaultChannelSize = 1024
)

// Bus is the default in-memory implementation of ports.EventBus.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[string][]ports.EventHandlerFunc
	closed      bool
	logger      *slog.Logger

	// Worker pool (bounded goroutines)
	events chan ports.Event
	workers int

	// Metrics (lock-free)
	published   atomic.Uint64
	delivered   atomic.Uint64
	dropped     atomic.Uint64
	handlerErrs atomic.Uint64
}

// New creates an empty EventBus with bounded worker pool.
func New(logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	b := &Bus{
		subscribers: make(map[string][]ports.EventHandlerFunc),
		logger:      logger,
		events:      make(chan ports.Event, defaultChannelSize),
		workers:     defaultWorkerCount,
	}
	b.startWorkers()
	return b
}

// startWorkers launches the fixed worker pool.
func (b *Bus) startWorkers() {
	for i := 0; i < b.workers; i++ {
		go b.worker()
	}
}

// worker processes events from the channel. Each worker is a bounded goroutine
// that never exits until Close() is called.
func (b *Bus) worker() {
	for evt := range b.events {
		b.mu.RLock()
		if b.closed {
			b.mu.RUnlock()
			return
		}
		// Collect handlers: exact topic match + wildcard subscribers
		var handlers []ports.EventHandlerFunc
		if h, ok := b.subscribers[evt.Topic]; ok {
			handlers = append(handlers, h...)
		}
		if h, ok := b.subscribers[""]; ok {
			handlers = append(handlers, h...)
		}
		b.mu.RUnlock()

		for _, h := range handlers {
			if err := h(context.Background(), evt); err != nil {
				b.handlerErrs.Add(1)
				b.logger.Warn("eventbus: handler error",
					slog.String("topic", evt.Topic),
					slog.String("type", evt.Type),
					slog.String("err", err.Error()))
			}
			b.delivered.Add(1)
		}
	}
}

// Publish sends an event to all subscribers of evt.Topic. NON-BLOCKING:
// if the internal channel is full, the event is dropped and ErrBusFull is
// returned. The caller MUST never block on this call.
func (b *Bus) Publish(_ context.Context, evt ports.Event) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrBusClosed
	}
	b.mu.RUnlock()

	// Non-blocking send: never block the caller
	select {
	case b.events <- evt:
		b.published.Add(1)
		return nil
	default:
		b.dropped.Add(1)
		b.logger.Warn("eventbus: channel full, event dropped",
			slog.String("topic", evt.Topic),
			slog.String("type", evt.Type))
		return ErrBusFull
	}
}

// Subscribe registers a handler for events on topic. Returns an
// unsubscribe function.
func (b *Bus) Subscribe(topic string, handler ports.EventHandlerFunc) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBusClosed
	}
	b.subscribers[topic] = append(b.subscribers[topic], handler)
	topicCopy := topic
	handlerIdx := len(b.subscribers[topic]) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if handlers, ok := b.subscribers[topicCopy]; ok {
			b.subscribers[topicCopy] = append(handlers[:handlerIdx], handlers[handlerIdx+1:]...)
		}
	}, nil
}

// Close stops the bus and waits for workers to drain.
func (b *Bus) Close(_ context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	close(b.events)
	return nil
}

// Stats returns lock-free metrics for monitoring.
func (b *Bus) Stats() map[string]uint64 {
	return map[string]uint64{
		"published":    b.published.Load(),
		"delivered":    b.delivered.Load(),
		"dropped":      b.dropped.Load(),
		"handler_errs": b.handlerErrs.Load(),
	}
}

var _ ports.EventBus = (*Bus)(nil)
