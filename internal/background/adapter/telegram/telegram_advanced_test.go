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
	"time"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

// recordingTelegram is a richer fake than the one in telegram_test.go: it captures
// EVERY Bot API call by method so adversarial paths (chunked Send, callback ACK,
// offset advance) can be asserted, and lets a test script the getUpdates body /
// status per call (malformed JSON, HTTP 429).
type recordingTelegram struct {
	mu sync.Mutex

	// inbound updates returned by getUpdates, filtered by the offset query param.
	updates []map[string]any

	// per-method call log.
	sends         []sendCall // sendMessage payloads, in order
	answerCBs     []map[string]any
	editMessages  []map[string]any
	offsetsSeen   []int // offset query param of each getUpdates, in order
	getUpdateHits int

	// optional scripted getUpdates responses (consumed front-to-back; falls through
	// to the default offset-filtered behaviour once exhausted).
	scripted []scriptedResp
}

type sendCall struct {
	chat string
	text string
}

type scriptedResp struct {
	status int
	body   string
}

func (f *recordingTelegram) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			f.getUpdateHits++
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			f.offsetsSeen = append(f.offsetsSeen, offset)
			if len(f.scripted) > 0 {
				sc := f.scripted[0]
				f.scripted = f.scripted[1:]
				if sc.status != 0 {
					w.WriteHeader(sc.status)
				}
				_, _ = io.WriteString(w, sc.body)
				return
			}
			var res []map[string]any
			for _, u := range f.updates {
				if toInt(u["update_id"]) >= offset {
					res = append(res, u)
				}
			}
			writeJSON(w, map[string]any{"ok": true, "result": res})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			chat, _ := m["chat_id"].(string)
			text, _ := m["text"].(string)
			f.sends = append(f.sends, sendCall{chat: chat, text: text})
			writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"message_id": len(f.sends)}})
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.answerCBs = append(f.answerCBs, m)
			writeJSON(w, map[string]any{"ok": true, "result": true})
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.editMessages = append(f.editMessages, m)
			writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// cbUpdate builds a getUpdates result that carries a callback_query (inline click).
func cbUpdate(id int, data string, fromID, chatID int64, msgID int) map[string]any {
	return map[string]any{
		"update_id": id,
		"callback_query": map[string]any{
			"id":   "cbq-" + strconv.Itoa(id),
			"data": data,
			"from": map[string]any{"id": fromID},
			"message": map[string]any{
				"message_id": msgID,
				"chat":       map[string]any{"id": chatID},
			},
		},
	}
}

func freshUpd(id int, chat int64, text string, date int64) map[string]any {
	return map[string]any{
		"update_id": id,
		"message":   map[string]any{"message_id": id, "text": text, "date": date, "chat": map[string]any{"id": chat}},
	}
}

func newAdapter(t *testing.T, f *recordingTelegram, cur CursorStore) (*Adapter, Provider) {
	srv := f.server(t)
	a := New([]Provider{{Name: "bot", Token: "T", CursorKey: "k", APIBase: srv.URL}}, cur, nil)
	return a, a.order[0]
}

// TestSendChunksLongReply : a reply over Telegram's 4096-char cap is split into
// multiple sendMessage calls, each within the limit, with the full content preserved.
func TestSendChunksLongReply(t *testing.T) {
	f := &recordingTelegram{}
	a, _ := newAdapter(t, f, &memCursors{m: map[string]string{}})

	// 10000 ASCII chars with spaces so the word-boundary breaker has cut points.
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("word ")
	}
	long := strings.TrimSpace(sb.String())
	if utf8.RuneCountInString(long) <= maxMessageChars {
		t.Fatalf("fixture too short: %d runes", utf8.RuneCountInString(long))
	}

	if err := a.Send(context.Background(), map[string]any{"chat_id": "9", "provider": "bot"}, long); err != nil {
		t.Fatalf("Send: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) < 2 {
		t.Fatalf("long reply not chunked: %d sendMessage calls", len(f.sends))
	}
	var reassembled strings.Builder
	for i, s := range f.sends {
		if n := utf8.RuneCountInString(s.text); n > maxMessageChars {
			t.Fatalf("chunk %d exceeds limit: %d runes", i, n)
		}
		if s.chat != "9" {
			t.Fatalf("chunk %d wrong chat: %q", i, s.chat)
		}
		if i > 0 {
			reassembled.WriteByte(' ')
		}
		reassembled.WriteString(s.text)
	}
	// Content preserved word-for-word (whitespace at break points is normalised).
	gotFields := strings.Fields(reassembled.String())
	wantFields := strings.Fields(long)
	if len(gotFields) != len(wantFields) {
		t.Fatalf("word count drift: got %d want %d", len(gotFields), len(wantFields))
	}
}

// TestSendChunkNeverSplitsRune : with a wall of multibyte runes and a small cap, every
// emitted chunk is valid UTF-8 — a codepoint is never cut in half.
func TestSendChunkNeverSplitsRune(t *testing.T) {
	// No spaces/newlines → forces a hard cut at the rune boundary (worst case).
	wall := strings.Repeat("é🚀漢", 5000) // multibyte, 3 runes per group
	chunks := chunkText(wall, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	var total int
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8 (rune split)", i)
		}
		if n := utf8.RuneCountInString(c); n > 100 {
			t.Fatalf("chunk %d over cap: %d runes", i, n)
		}
		total += utf8.RuneCountInString(c)
	}
	if total != utf8.RuneCountInString(wall) {
		t.Fatalf("rune count drift: got %d want %d", total, utf8.RuneCountInString(wall))
	}
}

// TestSendShortReplySingleCall : a sub-limit reply is one call, content verbatim.
func TestSendShortReplySingleCall(t *testing.T) {
	f := &recordingTelegram{}
	a, _ := newAdapter(t, f, &memCursors{m: map[string]string{}})
	if err := a.Send(context.Background(), map[string]any{"chat_id": "1", "provider": "bot"}, "hi there"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) != 1 || f.sends[0].text != "hi there" {
		t.Fatalf("short reply mis-sent: %+v", f.sends)
	}
}

// TestSendEmptyReplyNoCall : an empty/whitespace reply produces no API call (chunkText
// yields nothing) — no blank message spammed to the chat.
func TestSendEmptyReplyNoCall(t *testing.T) {
	f := &recordingTelegram{}
	a, _ := newAdapter(t, f, &memCursors{m: map[string]string{}})
	if err := a.Send(context.Background(), map[string]any{"chat_id": "1", "provider": "bot"}, "   \n  "); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) != 0 {
		t.Fatalf("blank reply produced %d sends", len(f.sends))
	}
}

// TestOffsetAdvancesNoReprocess : the cursor advances past consumed updates, persists,
// and a later poll using that cursor re-queries with the advanced offset (no replay).
func TestOffsetAdvancesNoReprocess(t *testing.T) {
	now := time.Now().Unix()
	f := &recordingTelegram{updates: []map[string]any{
		freshUpd(10, 100, "first", now),
		freshUpd(11, 100, "second", now),
	}}
	cur := &memCursors{m: map[string]string{"k": "10"}} // already armed → not first-arm
	a, p := newAdapter(t, f, cur)

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	a.pollOnce(context.Background(), p, sink)
	if len(got) != 2 {
		t.Fatalf("expected 2 emits, got %d", len(got))
	}
	if cur.Cursor(context.Background(), "k") != "12" {
		t.Fatalf("cursor not advanced: %q", cur.Cursor(context.Background(), "k"))
	}

	// Drain the fake (the offset would filter anyway) and re-poll: no reprocessing.
	got = nil
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 0 {
		t.Fatalf("re-poll reprocessed %d updates", len(got))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Second getUpdates must have used the advanced offset (12), proving the cursor
	// is fed back as the long-poll offset (consumed updates are never re-fetched).
	if len(f.offsetsSeen) != 2 || f.offsetsSeen[1] != 12 {
		t.Fatalf("offsets seen = %v, want second poll at 12", f.offsetsSeen)
	}
}

// TestCallbackRoutesAndAcks : a callback_query update routes to onCallback, which ACKs
// it (answerCallbackQuery) and edits the source message (editMessageText) to the
// verdict — even with no Prompt waiting (graceful, just the ACK + edit).
func TestCallbackRoutesAndAcks(t *testing.T) {
	now := time.Now().Unix()
	f := &recordingTelegram{updates: []map[string]any{
		cbUpdate(20, "a:deadbeef:0", 77, 100, 555),
		freshUpd(21, 100, "after", now),
	}}
	cur := &memCursors{m: map[string]string{"k": "20"}}
	a, p := newAdapter(t, f, cur)

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	a.pollOnce(context.Background(), p, sink)

	// The callback is NOT a chat message; only the fresh text message becomes an event.
	if len(got) != 1 || got[0].Message != "after" {
		t.Fatalf("callback leaked as a message event: %+v", got)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answerCBs) != 1 {
		t.Fatalf("callback not ACKed: %d answerCallbackQuery", len(f.answerCBs))
	}
	if id, _ := f.answerCBs[0]["callback_query_id"].(string); id != "cbq-20" {
		t.Fatalf("ACK wrong query id: %q", id)
	}
	if len(f.editMessages) != 1 {
		t.Fatalf("source message not edited: %d editMessageText", len(f.editMessages))
	}
	// Edit must target the callback's own message.
	if toInt(f.editMessages[0]["message_id"]) != 555 {
		t.Fatalf("edit targeted wrong message: %v", f.editMessages[0]["message_id"])
	}
}

// TestCallbackMalformedDataGraceful : a callback with junk callback_data still ACKs
// (stops the client spinner) but is otherwise ignored — no panic, no edit nonsense
// beyond a best-effort verdict, no crash.
func TestCallbackMalformedDataGraceful(t *testing.T) {
	f := &recordingTelegram{}
	a, p := newAdapter(t, f, &memCursors{m: map[string]string{}})

	for _, data := range []string{"", "garbage", "a:onlytwo", "a:nonce:notanint", "x:nonce:0"} {
		a.onCallback(context.Background(), p, &callbackQuery{
			ID:   "cbX",
			Data: data,
			From: struct {
				ID int64 `json:"id"`
			}{ID: 1},
		})
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Each malformed callback is still ACKed (UX: kill the spinner) and never panics.
	if len(f.answerCBs) != 5 {
		t.Fatalf("malformed callbacks not all ACKed: %d", len(f.answerCBs))
	}
}

// TestFirstArmSkipsStaleDeliversFresh : on a fresh cursor a backlog message older than
// the freshness window is skipped while a just-sent message is delivered; the cursor
// still advances past BOTH so neither is re-fetched.
func TestFirstArmSkipsStaleDeliversFresh(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute).Unix() // well outside firstArmFreshWindow
	fresh := time.Now().Unix()
	f := &recordingTelegram{updates: []map[string]any{
		freshUpd(30, 100, "stale backlog", old),
		freshUpd(31, 100, "just sent", fresh),
	}}
	cur := &memCursors{m: map[string]string{}} // empty → first arm
	a, p := newAdapter(t, f, cur)

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	a.pollOnce(context.Background(), p, sink)
	if len(got) != 1 || got[0].Message != "just sent" {
		t.Fatalf("first-arm freshness wrong: %+v", got)
	}
	if cur.Cursor(context.Background(), "k") != "32" {
		t.Fatalf("cursor must advance past both updates: %q", cur.Cursor(context.Background(), "k"))
	}
}

// TestFirstArmZeroDateSkipped : a backlog message with no date stamp (date=0) is
// treated as non-fresh and skipped on first arm (recent() guards date>0).
func TestFirstArmZeroDateSkipped(t *testing.T) {
	f := &recordingTelegram{updates: []map[string]any{
		freshUpd(40, 100, "no date", 0),
	}}
	cur := &memCursors{m: map[string]string{}}
	a, p := newAdapter(t, f, cur)

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 0 {
		t.Fatalf("date=0 backlog should be skipped on first arm, got %+v", got)
	}
	if cur.Cursor(context.Background(), "k") != "41" {
		t.Fatalf("cursor must still advance: %q", cur.Cursor(context.Background(), "k"))
	}
}

// TestGetUpdatesMalformedPayloads : malformed bodies (broken JSON, ok=false, a
// non-object update inside a valid envelope) never panic and never advance the cursor
// past unseen updates; the poll degrades to a no-op.
func TestGetUpdatesMalformedPayloads(t *testing.T) {
	cases := []scriptedResp{
		{body: "this is not json{{{"},                          // body parse fails → getUpdates err
		{body: `{"ok":false,"description":"bot was blocked"}`}, // ok=false → err
		{body: `{"ok":true,"result":[ 12345 ]}`},               // element not an object → skipped, no panic
		{body: `{"ok":true}`},                                  // missing result → empty
		{body: `{"ok":true,"result":[]}`},                      // explicit empty
	}
	f := &recordingTelegram{scripted: cases}
	cur := &memCursors{m: map[string]string{"k": "5"}} // armed at offset 5
	a, p := newAdapter(t, f, cur)

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	for range cases {
		a.pollOnce(context.Background(), p, sink) // must not panic
	}
	if len(got) != 0 {
		t.Fatalf("malformed payloads produced %d events", len(got))
	}
	// Cursor must not have regressed or jumped past unseen ids; it stays at 5 because
	// no update with id >= 5 was ever successfully parsed.
	if cur.Cursor(context.Background(), "k") != "5" {
		t.Fatalf("cursor drifted on malformed input: %q", cur.Cursor(context.Background(), "k"))
	}
}

// TestGetUpdates429NoPanic : an HTTP 429 (rate limit) from getUpdates is surfaced as an
// error, the poll is a no-op (cursor untouched), and a subsequent successful poll then
// makes progress — the loop recovers.
func TestGetUpdates429NoPanic(t *testing.T) {
	now := time.Now().Unix()
	f := &recordingTelegram{
		scripted: []scriptedResp{
			{status: http.StatusTooManyRequests, body: `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`},
		},
		updates: []map[string]any{freshUpd(50, 100, "after backoff", now)},
	}
	cur := &memCursors{m: map[string]string{"k": "50"}}
	a, p := newAdapter(t, f, cur)

	var got []adapter.Event
	sink := func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	// First poll hits the 429 → no panic, no emit, cursor untouched.
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 0 {
		t.Fatalf("429 poll emitted %d", len(got))
	}
	if cur.Cursor(context.Background(), "k") != "50" {
		t.Fatalf("cursor moved on 429: %q", cur.Cursor(context.Background(), "k"))
	}

	// Scripted 429 consumed → next poll falls through to the real update and recovers.
	a.pollOnce(context.Background(), p, sink)
	if len(got) != 1 || got[0].Message != "after backoff" {
		t.Fatalf("did not recover after 429: %+v", got)
	}
	if cur.Cursor(context.Background(), "k") != "51" {
		t.Fatalf("cursor not advanced after recovery: %q", cur.Cursor(context.Background(), "k"))
	}
}

// TestPromptCtxCancelReturnsErr : if the Prompt context ends before any click, the
// blocked Prompt unblocks and returns the context error (the processor then degrades /
// resolves the decision elsewhere — it never hangs forever).
func TestPromptCtxCancelReturnsErr(t *testing.T) {
	f := &recordingTelegram{}
	a, _ := newAdapter(t, f, &memCursors{m: map[string]string{}})

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		r adapter.PromptResponse
		e error
	}
	ch := make(chan res, 1)
	go func() {
		r, e := a.Prompt(ctx, adapter.PromptRequest{
			ReplyRef: map[string]any{"provider": "bot", "chat_id": "1"},
			Title:    "Approve?",
			Options:  []adapter.PromptOption{{ID: "ok", Label: "OK"}},
		})
		ch <- res{r, e}
	}()

	// Wait until the prompt message is posted, then cancel; the prompt must unblock.
	for range 1000 {
		f.mu.Lock()
		posted := len(f.sends) > 0
		f.mu.Unlock()
		if posted {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case got := <-ch:
		if got.e == nil {
			t.Fatal("cancelled prompt must return an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled prompt never returned")
	}
}

// TestExpirePromptDropsKeyboard : the keyboard-drop on prompt expiry is a single
// editMessageText that rewrites the message and clears the inline keyboard. Tested
// directly (no ctx race): expirePrompt is what the cancel branch invokes.
func TestExpirePromptDropsKeyboard(t *testing.T) {
	f := &recordingTelegram{}
	a, p := newAdapter(t, f, &memCursors{m: map[string]string{}})

	a.expirePrompt(p, "777", 99)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.editMessages) != 1 {
		t.Fatalf("expirePrompt must edit exactly once, got %d", len(f.editMessages))
	}
	e := f.editMessages[0]
	if toInt(e["message_id"]) != 99 {
		t.Fatalf("edit targeted wrong message: %v", e["message_id"])
	}
	// chat_id is sent as the numeric form (ParseInt of the string handle).
	if toInt(e["chat_id"]) != 777 {
		t.Fatalf("edit targeted wrong chat: %v", e["chat_id"])
	}
	km, ok := e["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("expiry edit carried no reply_markup: %#v", e["reply_markup"])
	}
	kb, ok := km["inline_keyboard"].([]any)
	if !ok || len(kb) != 0 {
		t.Fatalf("expiry must clear the inline keyboard, got %#v", km["inline_keyboard"])
	}
}

// TestExpirePromptZeroMsgIDNoCall : with no message id (post never returned one) the
// expiry is a no-op — no stray editMessageText against message 0.
func TestExpirePromptZeroMsgIDNoCall(t *testing.T) {
	f := &recordingTelegram{}
	a, p := newAdapter(t, f, &memCursors{m: map[string]string{}})
	a.expirePrompt(p, "1", 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.editMessages) != 0 {
		t.Fatalf("zero-msgid expiry should not edit, got %d", len(f.editMessages))
	}
}
