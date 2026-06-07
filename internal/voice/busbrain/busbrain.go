// Package busbrain is the concrete enginebrain.Brain: it drives a real daemon turn
// by appending the caller's text as a user message, triggering the turn, and
// streaming the assistant token-deltas off the session bus (EventAssistantDelta)
// until the turn ends (EventTurnEnded). It is the binding between the voice pipeline
// and the daemon's own engine — the daemon IS the brain, gateway LLM + tools + gates
// + memory all apply, identical to a typed message.
//
// It depends only on sessionstore (the Event types) + injected closures, so the
// daemon-core wiring (AppendDurable, bus.Subscribe, the sessionRunner's wake/abort)
// is supplied at bootstrap and this stays unit-testable with plain fakes.
package busbrain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/voice/enginebrain"
)

// Deps are the daemon-core bindings, supplied at bootstrap.
type Deps struct {
	// AppendUserMessage durably appends the caller's text as a user message on the
	// bound session (→ sessionStore.AppendDurable(EventUserMessage)).
	AppendUserMessage func(ctx context.Context, text string) error
	// Subscribe registers cb for the bound session's events and returns an
	// unsubscribe closure (→ bus.Subscribe(sessionID, cb) + sub.Close).
	Subscribe func(cb func(sessionstore.Event)) (unsub func(), err error)
	// Trigger wakes a turn on the bound session (→ sessionRunner.WakeTurn).
	Trigger func()
	// Abort interrupts the bound session's in-flight turn (→ sessionRunner.Abort).
	Abort func()
}

// Brain implements enginebrain.Brain for one call's session.
type Brain struct {
	deps Deps
}

func New(deps Deps) *Brain { return &Brain{deps: deps} }

// StartTurn subscribes to the session's events, appends the user message, and wakes
// the turn. The returned Turn streams the assistant deltas until turn-end.
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

// Abort interrupts the in-flight turn (voice barge-in).
func (b *Brain) Abort(_ context.Context) error {
	if b.deps.Abort != nil {
		b.deps.Abort()
	}
	return nil
}

// busTurn bridges one turn's bus events to a delta channel.
type busTurn struct {
	deltas  chan string
	closed  chan struct{}
	unsub   func()
	once    sync.Once
	relayed atomic.Bool // a token/message has been forwarded for this turn

	mu  sync.Mutex
	err error
}

func newBusTurn() *busTurn {
	return &busTurn{deltas: make(chan string, 256), closed: make(chan struct{})}
}

// onEvent runs on the bus delivery goroutine: it forwards assistant deltas and ends
// the turn on EventTurnEnded. A blocking send (guarded by closed) never drops a token
// yet never wedges the bus once the turn is over.
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
		// Non-streaming fallback: when the engine ran a synchronous turn (no per-
		// token deltas), the whole reply arrives here once. Relay it — unless deltas
		// already carried it, in which case re-speaking would duplicate the audio.
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

// forward delivers text to the delta stream without blocking the bus goroutine past
// the turn's end.
func (t *busTurn) forward(txt string) {
	select {
	case t.deltas <- txt:
	case <-t.closed:
	}
}

// partsText concatenates the text parts of an assistant_delta message.
func partsText(parts []sessionstore.MessagePart) string {
	var b strings.Builder
	for i := range parts {
		b.WriteString(parts[i].Text)
	}
	return b.String()
}

// messageText returns the full text of a final assistant message (Content for the
// non-streaming path, falling back to concatenated Parts).
func messageText(m *sessionstore.MessagePayload) string {
	if m.Content != "" {
		return m.Content
	}
	return partsText(m.Parts)
}

var _ enginebrain.Brain = (*Brain)(nil)
