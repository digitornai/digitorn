// Package adapter is the transport seam of the background service. Every
// inbound source (webhook, cron, rss, email, whatsapp, telegram, voice…) is
// "just an Adapter": it turns external deliveries into Events and pushes them to
// a durable Sink, and optionally delivers replies back out. The core depends
// only on this interface, so a new transport is one file with no core change.
package adapter

import "context"

// Event is one inbound delivery, transport-agnostic and pre-pipeline. The
// durable intake records it; the channel pipeline later turns it into a session
// launch. DedupKey makes intake idempotent (a redelivery is dropped).
type Event struct {
	Provider string         // provider instance name (the configured channel)
	Adapter  string         // adapter type (webhook, cron, …) — used to route replies
	DedupKey string         // stable per delivery: message-id / delivery-id / tick
	Source   string         // sender: ip / email / user_id / phone
	Message  string         // text content, if any
	Payload  map[string]any // raw event body
	Metadata map[string]any // transport metadata (headers, etc.)
	ReplyRef map[string]any // opaque handle to answer the originator (reply:auto)
}

// Sink is the durable intake: an adapter calls it for every delivery, BEFORE it
// ACKs the source, so a crash never loses an in-flight event.
type Sink func(ctx context.Context, ev Event) error

// Adapter is one transport. Start is long-lived (webhook/voice register HTTP
// handlers; cron/rss/imap run a poll loop) and pushes events until ctx is
// cancelled. Send delivers a reply on this transport (no-op for inbound-only).
type Adapter interface {
	Name() string
	Start(ctx context.Context, sink Sink) error
	Send(ctx context.Context, replyRef map[string]any, text string) error
}

// Registry maps adapter-type names to their implementations, so the reply path
// can find the adapter that delivered an event.
type Registry struct{ byName map[string]Adapter }

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{byName: map[string]Adapter{}} }

// Register adds an adapter under its Name(). Last registration wins.
func (r *Registry) Register(a Adapter) { r.byName[a.Name()] = a }

// Get returns the adapter for a type name, or nil.
func (r *Registry) Get(name string) Adapter { return r.byName[name] }

// All returns every registered adapter (for the manager to Start them).
func (r *Registry) All() []Adapter {
	out := make([]Adapter, 0, len(r.byName))
	for _, a := range r.byName {
		out = append(out, a)
	}
	return out
}
