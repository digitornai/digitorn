package discord

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

// TestOnMessage : a human message becomes one Event with the right fields ; a bot
// message (incl. our own reply) and an empty message are dropped — the bot-filter is
// what prevents an infinite reply loop.
func TestOnMessage(t *testing.T) {
	a := New([]Provider{{Name: "dc", Token: "x"}}, nil)
	p := a.byName["dc"]

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }
	emit := func(j string) { a.onMessage(context.Background(), p, json.RawMessage(j), sink) }

	emit(`{"id":"1","channel_id":"c9","content":"hello bot","author":{"id":"u1","bot":false}}`)
	emit(`{"id":"2","channel_id":"c9","content":"my own reply","author":{"id":"b1","bot":true}}`)
	emit(`{"id":"3","channel_id":"c9","content":"","author":{"id":"u1","bot":false}}`)

	if len(got) != 1 {
		t.Fatalf("expected 1 event (bot + empty filtered), got %d", len(got))
	}
	e := got[0]
	if e.Adapter != "discord" || e.Source != "c9" || e.Message != "hello bot" || e.DedupKey != "dc:1" {
		t.Fatalf("event mapping wrong: %+v", e)
	}
	if e.ReplyRef["channel_id"] != "c9" || e.ReplyRef["provider"] != "dc" {
		t.Fatalf("reply ref wrong: %+v", e.ReplyRef)
	}
}
