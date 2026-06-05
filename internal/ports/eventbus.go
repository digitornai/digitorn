package ports

import "context"

// Event is a typed message published on the bus.
type Event struct {
	Topic    string         `json:"topic"`
	Type     string         `json:"type"`
	Source   string         `json:"source,omitempty"`
	Data     any            `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// EventHandler processes an event. Returning an error logs but does not stop
// other subscribers from receiving the event.
type EventHandlerFunc func(ctx context.Context, evt Event) error

// EventBus is an in-process pub/sub bus used by modules and the runtime to
// communicate asynchronously without direct coupling.
type EventBus interface {
	// Publish sends an event to all subscribers of the topic. Non-blocking;
	// delivery is async. Returns an error only if the bus is closed.
	Publish(ctx context.Context, evt Event) error

	// Subscribe registers a handler for events on a topic. Returns an
	// unsubscribe function.
	Subscribe(topic string, handler EventHandlerFunc) (unsubscribe func(), err error)

	// Close stops the bus and waits for in-flight handlers to finish.
	Close(ctx context.Context) error
}
