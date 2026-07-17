package busbrain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/voice/enginebrain"
)

type Deps struct {
	AppendUserMessage func(ctx context.Context, text string) error
	Subscribe func(cb func(sessionstore.Event)) (unsub func(), err error)
	Trigger func()
	Abort func()
}

type Brain struct {
	deps Deps
}

func New(deps Deps) *Brain { return &Brain{deps: deps} }

func (b *Brain) StartTurn(ctx context.Context, text string) (enginebrain.Turn, error) {
	t := newBusTurn()
	if b.deps.Subscribe != nil {
		unsub, err := b.deps.Subscribe(t.onEvent)
		if err != nil {
			return nil, err
		}
		t.unsub = unsub
	}
	if b.deps.AppendUserMessage != nil {
		if err := b.deps.AppendUserMessage(ctx, text); err != nil {
			t.finish(nil)
			return nil, err
		}
	}
	if b.deps.Trigger != nil {
		b.deps.Trigger()
	}
	return t, nil
}

func (b *Brain) Abort(_ context.Context) error {
	if b.deps.Abort != nil {
		b.deps.Abort()
	}
	return nil
}

type busTurn struct {
	deltas  chan string
	closed  chan struct{}
	unsub   func()
	once    sync.Once
	relayed atomic.Bool

	mu  sync.Mutex
	err error
}

func newBusTurn() *busTurn {
	return &busTurn{deltas: make(chan string, 256), closed: make(chan struct{})}
}

func (t *busTurn) onEvent(ev sessionstore.Event) {
	switch ev.Type {
	case sessionstore.EventAssistantDelta:
		if ev.Message == nil {
			return
		}
		if txt := partsText(ev.Message.Parts); txt != "" {
			t.relayed.Store(true)
			t.forward(txt)
		}
	case sessionstore.EventAssistantMessage:
		if t.relayed.Load() || ev.Message == nil {
			return
		}
		if txt := messageText(ev.Message); txt != "" {
			t.relayed.Store(true)
			t.forward(txt)
		}
	case sessionstore.EventTurnEnded:
		var err error
		if ev.Turn != nil && ev.Turn.Status == "errored" {
			reason := ev.Turn.Reason
			if reason == "" {
				reason = "turn errored"
			}
			err = errors.New(reason)
		}
		t.finish(err)
	}
}

func (t *busTurn) finish(err error) {
	t.once.Do(func() {
		if err != nil {
			t.mu.Lock()
			t.err = err
			t.mu.Unlock()
		}
		close(t.closed)
		if t.unsub != nil {
			t.unsub()
		}
		close(t.deltas)
	})
}

func (t *busTurn) Deltas() <-chan string { return t.deltas }

func (t *busTurn) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *busTurn) forward(txt string) {
	select {
	case t.deltas <- txt:
	case <-t.closed:
	}
}

func partsText(parts []sessionstore.MessagePart) string {
	var b strings.Builder
	for i := range parts {
		b.WriteString(parts[i].Text)
	}
	return b.String()
}

func messageText(m *sessionstore.MessagePayload) string {
	if m.Content != "" {
		return m.Content
	}
	return partsText(m.Parts)
}

var _ enginebrain.Brain = (*Brain)(nil)
