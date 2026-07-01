package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

type memCursors struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *memCursors) Cursor(_ context.Context, k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}
func (c *memCursors) SetCursor(_ context.Context, k, v string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
	return nil
}

// fakeTelegram mocks the Bot API: getUpdates honours the offset (returns updates
// with update_id >= offset) and sendMessage records the call.
type fakeTelegram struct {
	mu       sync.Mutex
	updates  []map[string]any // each {update_id, message:{text, chat:{id}}}
	sentChat string
	sentText string
}

func (f *fakeTelegram) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			var res []map[string]any
			for _, u := range f.updates {
				if int(u["update_id"].(int)) >= offset {
					res = append(res, u)
				}
			}
			writeJSON(w, map[string]any{"ok": true, "result": res})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			f.sentChat, _ = m["chat_id"].(string)
			f.sentText, _ = m["text"].(string)
			writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func upd(id int, chat int64, text string) map[string]any {
	return map[string]any{
		"update_id": id,
		"message":   map[string]any{"message_id": id, "text": text, "chat": map[string]any{"id": chat}},
	}
}

func TestTelegram_InboundOffsetAndFirstArm(t *testing.T) {
	f := &fakeTelegram{updates: []map[string]any{upd(5, 100, "old1"), upd(6, 100, "old2")}}
	srv := f.server(t)
	cur := &memCursors{m: map[string]string{}}
	a := New([]Provider{{Name: "bot", Token: "T", CursorKey: "k", APIBase: srv.URL}}, cur, nil)
	p := a.order[0]
	p.APIBase = srv.URL // New copied defaults into byName/order

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	// First arm: ack the backlog (5,6) WITHOUT replaying. Cursor → 7.
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 0 {
		t.Fatalf("first arm replayed backlog: %d", len(got))
	}
	if cur.Cursor(context.Background(), "k") != "7" {
		t.Fatalf("cursor after first arm = %q, want 7", cur.Cursor(context.Background(), "k"))
	}

	// A new message arrives → emitted with a reply handle; cursor → 8.
	f.mu.Lock()
	f.updates = append(f.updates, upd(7, 100, "hello bot"))
	f.mu.Unlock()
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 1 || got[0].Message != "hello bot" || got[0].Adapter != "telegram" {
		t.Fatalf("inbound emit wrong: %+v", got)
	}
	if got[0].ReplyRef["chat_id"] != "100" || got[0].ReplyRef["provider"] != "bot" {
		t.Fatalf("reply handle wrong: %+v", got[0].ReplyRef)
	}
	if got[0].DedupKey != "bot:7" {
		t.Fatalf("dedup key = %q", got[0].DedupKey)
	}
	if cur.Cursor(context.Background(), "k") != "8" {
		t.Fatalf("cursor = %q, want 8", cur.Cursor(context.Background(), "k"))
	}

	// Re-poll, nothing new → no emit.
	got = nil
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 0 {
		t.Fatalf("re-poll emitted %d", len(got))
	}
}

func TestTelegram_OutboundSend(t *testing.T) {
	f := &fakeTelegram{}
	srv := f.server(t)
	a := New([]Provider{{Name: "bot", Token: "T", CursorKey: "k", APIBase: srv.URL}}, &memCursors{m: map[string]string{}}, nil)

	err := a.Send(context.Background(), map[string]any{"chat_id": "555", "provider": "bot"}, "the answer")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f.sentChat != "555" || f.sentText != "the answer" {
		t.Fatalf("sendMessage got chat=%q text=%q", f.sentChat, f.sentText)
	}

	// Unknown provider → error (no token to use).
	if err := a.Send(context.Background(), map[string]any{"chat_id": "1", "provider": "ghost"}, "x"); err == nil {
		t.Fatal("send to unknown provider should error")
	}
}
