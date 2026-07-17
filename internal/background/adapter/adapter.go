package adapter

import "context"

type Event struct {
	Provider string
	Adapter  string
	DedupKey string
	Source   string
	Message  string
	Payload  map[string]any
	Metadata map[string]any
	ReplyRef map[string]any
	Attachments []Attachment
}

type Attachment struct {
	Filename    string
	ContentType string
	Size        int64
	Ref         map[string]any
}

type MediaFetcher interface {
	FetchMedia(ctx context.Context, att Attachment) (data []byte, mime string, err error)
}

type Sink func(ctx context.Context, ev Event) error

type Adapter interface {
	Name() string
	Start(ctx context.Context, sink Sink) error
	Send(ctx context.Context, replyRef map[string]any, text string) error
}

type Typer interface {
	Typing(ctx context.Context, replyRef map[string]any) error
}

type Prompter interface {
	Prompt(ctx context.Context, req PromptRequest) (PromptResponse, error)
}

type PromptRequest struct {
	ReplyRef        map[string]any
	Title           string
	Body            string
	Options         []PromptOption
	AllowText       bool
	TextLabel       string
	TextPlaceholder string
	Multiline       bool
}

type PromptOption struct {
	ID    string
	Label string
	Style string
}

type PromptResponse struct {
	OptionID string
	Text     string
	UserID   string
}

type Registry struct{ byName map[string]Adapter }

func NewRegistry() *Registry { return &Registry{byName: map[string]Adapter{}} }

func (r *Registry) Register(a Adapter) { r.byName[a.Name()] = a }

func (r *Registry) Get(name string) Adapter { return r.byName[name] }

func (r *Registry) All() []Adapter {
	out := make([]Adapter, 0, len(r.byName))
	for _, a := range r.byName {
		out = append(out, a)
	}
	return out
}
