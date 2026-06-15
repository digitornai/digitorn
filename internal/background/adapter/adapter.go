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
	// Attachments are inbound media the user sent with the message (image, doc, …).
	// The processor fetches each (via the adapter's MediaFetcher) and uploads it to
	// the daemon so the model sees it as vision/audio content.
	Attachments []Attachment
}

// Attachment is a generic inbound file reference, transport-agnostic. The bytes are
// fetched lazily by the SAME adapter (which knows the channel's media auth) via
// MediaFetcher — only the metadata + an opaque per-adapter Ref travel on the event.
type Attachment struct {
	Filename    string
	ContentType string         // mime, e.g. image/png (best-effort)
	Size        int64          // bytes, if known
	Ref         map[string]any // opaque handle the adapter uses to download (URL, file_id, …)
}

// MediaFetcher is an OPTIONAL adapter capability: download an inbound attachment's
// bytes, applying the channel's own media auth (Discord CDN GET, Telegram getFile +
// token, WhatsApp media API, …). The processor calls it before launching so the
// daemon receives real bytes. Adapters without media simply don't implement it.
type MediaFetcher interface {
	FetchMedia(ctx context.Context, att Attachment) (data []byte, mime string, err error)
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

// Typer is an OPTIONAL adapter capability: emit a transient "typing"/presence hint
// on a channel while the agent composes its reply, so the user sees activity instead
// of dead silence during a long (tool-using) turn. Adapters that can't express
// presence (cron, rss, webhook) simply don't implement it — the processor no-ops for
// them. Generic: every chat channel (Discord, Telegram, WhatsApp, Signal, …) that
// implements Typing gets the behaviour with zero processor changes.
type Typer interface {
	Typing(ctx context.Context, replyRef map[string]any) error
}

// Prompter is an OPTIONAL adapter capability: surface a human-in-the-loop decision
// IN the channel and block until the user answers. It backs both flavours of the
// daemon's synchronous pause: a gated tool awaiting approval (Approve / Reject) and
// an ask_user question (one button per choice, or a free-text input). The adapter
// renders native controls (Discord buttons / a modal, Telegram inline keyboard, …),
// waits for the user's interaction, and returns the chosen option (or typed text).
//
// Generic + future-proof: the processor drives this against ANY channel that
// implements it; channels that can't (cron, rss, webhook) simply don't, and the
// approval stays resolvable via the web/CLI (or times out) — graceful degradation,
// zero per-channel code in the processor.
type Prompter interface {
	Prompt(ctx context.Context, req PromptRequest) (PromptResponse, error)
}

// PromptRequest is one decision to put to the user. ReplyRef is the SAME opaque
// channel handle used by Send/Typing (so the prompt lands in the originating
// conversation). Options are the discrete choices (rendered as buttons); when
// AllowText is set the adapter also offers a free-text input (a modal, for an
// ask_user question with no/optional choices). Title is a short header, Body the
// detail (tool name + params, or the question text).
type PromptRequest struct {
	ReplyRef        map[string]any
	Title           string
	Body            string
	Options         []PromptOption
	AllowText       bool   // also accept a free-text answer (ask_user free/optional input)
	TextLabel       string // label for the text input (e.g. "Your answer")
	TextPlaceholder string // placeholder hint for the text input
	Multiline       bool   // text input spans multiple lines
}

// PromptOption is one selectable choice. ID is the value the processor maps back to
// a resolve action/answer (never shown). Style is a UI hint the adapter maps to its
// own palette ("primary" | "danger" | "secondary"); unknown styles fall back.
type PromptOption struct {
	ID    string
	Label string
	Style string
}

// PromptResponse is the user's answer. Exactly one of OptionID / Text is set: the
// chosen option's ID, or the free-text the user typed. UserID is the channel-native
// id of whoever answered, carried for the audit trail.
type PromptResponse struct {
	OptionID string
	Text     string
	UserID   string
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
