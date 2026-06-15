package daemonclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptDaemon serves a SCRIPTED sequence of /history responses — one per poll —
// so a test can model exactly how the real async daemon surfaces a turn: events
// trickling in across polls, turn_ended timing, and transient/permanent faults.
// Each GET /history returns the next scripted poll; the final poll repeats so a
// client that needs one more read to observe completion always can.
type scriptDaemon struct {
	mu    sync.Mutex
	polls []scriptPoll
	hits  int
}

type scriptPoll struct {
	status int            // 0 → 200
	body   map[string]any // history payload (events/messages/turn_active)
}

func ev(seq int, typ string, content string) map[string]any {
	p := map[string]any{}
	if content != "" {
		p["content"] = content
	}
	return map[string]any{"seq": seq, "type": typ, "payload": p}
}

func toolEv(seq int, name, status string) map[string]any {
	return map[string]any{"seq": seq, "type": "tool_result", "payload": map[string]any{"name": name, "status": status}}
}

func (s *scriptDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/history") {
			writeJSONT(w, 404, map[string]any{"code": "not_found"})
			return
		}
		s.mu.Lock()
		i := s.hits
		if i >= len(s.polls) {
			i = len(s.polls) - 1
		}
		s.hits++
		pl := s.polls[i]
		s.mu.Unlock()
		writeJSONT(w, statusOr(pl.status, 200), pl.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hist(turnActive bool, events ...map[string]any) map[string]any {
	return map[string]any{"turn_active": turnActive, "events": events}
}

func streamClient(t *testing.T, polls ...scriptPoll) *Client {
	d := &scriptDaemon{polls: polls}
	return New(d.server(t).URL, "", WithPollInterval(time.Millisecond))
}

func collect(t *testing.T, c *Client) ([]StreamItem, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []StreamItem
	err := c.StreamReplies(ctx, "app", "sess", 0, func(it StreamItem) { got = append(got, it) })
	return got, err
}

// Events arriving over four separate polls must be relayed in seq order, exactly
// once each, with the right kind, and the stream must end on the durable
// turn_ended — never before.
func TestStream_TrickleAcrossPolls_OrderedNoDups(t *testing.T) {
	c := streamClient(t,
		scriptPoll{body: hist(false, ev(3, "turn_started", ""))},
		scriptPoll{body: hist(false, ev(4, "assistant_message", "Je regarde le dossier…"))},
		scriptPoll{body: hist(false, toolEv(5, "filesystem.glob", "ok"))},
		scriptPoll{body: hist(false, ev(6, "assistant_message", "3 fichiers trouvés."), ev(7, "turn_ended", ""))},
	)
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	want := []StreamItem{
		{Seq: 4, Kind: "message", Text: "Je regarde le dossier…"},
		{Seq: 5, Kind: "tool", Text: toolLine("filesystem.glob", "ok")},
		{Seq: 6, Kind: "message", Text: "3 fichiers trouvés."},
	}
	if len(got) != len(want) {
		t.Fatalf("item count: got %d want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// A tool-only turn (the model runs a tool then ends with no closing prose) must
// still complete on turn_ended and relay only the tool line.
func TestStream_ToolOnlyTurn_Completes(t *testing.T) {
	c := streamClient(t,
		scriptPoll{body: hist(false, ev(3, "turn_started", ""))},
		scriptPoll{body: hist(false, toolEv(4, "filesystem.write", "ok"), ev(5, "turn_ended", ""))},
	)
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "tool" {
		t.Fatalf("want a single tool item, got %+v", got)
	}
}

// Pre-existing events from a since=0 scan (session_started / user_message /
// model_changed) must NOT mark the turn started — otherwise the stream would
// conclude "done" before the turn runs. Only after real turn events arrive and
// turn_ended lands does it complete, relaying just the real assistant text.
func TestStream_PreExistingEventsDontCount(t *testing.T) {
	c := streamClient(t,
		scriptPoll{body: hist(false,
			map[string]any{"seq": 1, "type": "session_started"},
			map[string]any{"seq": 2, "type": "user_message", "payload": map[string]any{"content": "salut"}},
			map[string]any{"seq": 3, "type": "model_changed"},
		)},
		scriptPoll{body: hist(false, ev(4, "assistant_message", "Bonjour !"), ev(5, "turn_ended", ""))},
	)
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(got) != 1 || got[0].Text != "Bonjour !" {
		t.Fatalf("must relay only the real assistant text, got %+v", got)
	}
}

// An empty assistant_message (a common terminal artifact) must be skipped, never
// relayed as a blank channel message.
func TestStream_EmptyAssistantMessageSkipped(t *testing.T) {
	c := streamClient(t,
		scriptPoll{body: hist(false,
			ev(4, "assistant_message", "Réponse."),
			ev(5, "assistant_message", ""),
			ev(6, "turn_ended", ""),
		)},
	)
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(got) != 1 || got[0].Text != "Réponse." {
		t.Fatalf("empty message must be skipped, got %+v", got)
	}
}

// A transient 503 mid-stream is retried — the events that follow are still relayed.
func TestStream_TransientErrorRecovers(t *testing.T) {
	c := streamClient(t,
		scriptPoll{status: 503, body: map[string]any{"code": "unavailable", "error": "later"}},
		scriptPoll{body: hist(false, ev(4, "assistant_message", "OK"), ev(5, "turn_ended", ""))},
	)
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("stream should recover from 503: %v", err)
	}
	if len(got) != 1 || got[0].Text != "OK" {
		t.Fatalf("post-recovery item missing: %+v", got)
	}
}

// A permanent 400 aborts the stream with an error (retrying would just fail).
func TestStream_PermanentErrorAborts(t *testing.T) {
	c := streamClient(t,
		scriptPoll{status: 400, body: map[string]any{"code": "bad_request", "error": "nope"}},
	)
	got, err := collect(t, c)
	if err == nil {
		t.Fatalf("permanent 400 must abort with error; got items=%v", got)
	}
	if len(got) != 0 {
		t.Fatalf("no items expected on abort, got %+v", got)
	}
}

// ctx cancellation stops the stream promptly without error.
func TestStream_CtxCancelStops(t *testing.T) {
	d := &scriptDaemon{polls: []scriptPoll{{body: hist(true, ev(3, "turn_started", ""))}}} // never ends
	c := New(d.server(t).URL, "", WithPollInterval(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = c.StreamReplies(ctx, "app", "sess", 0, func(StreamItem) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamReplies did not stop on ctx cancel")
	}
}

// ── WaitForReply (reply:auto) under the same adversarial conditions ──────────

func waitClient(t *testing.T, polls ...scriptPoll) *Client {
	d := &scriptDaemon{polls: polls}
	return New(d.server(t).URL, "", WithPollInterval(time.Millisecond))
}

// The preamble arrives early, the final answer late: WaitForReply must return the
// FINAL answer (keyed on turn_ended), never the preamble — even across polls.
func TestWaitReply_PreambleThenFinalAcrossPolls(t *testing.T) {
	c := waitClient(t,
		scriptPoll{body: hist(false, ev(4, "assistant_message", "Laisse-moi regarder…"))},
		scriptPoll{body: hist(false, toolEv(5, "filesystem.glob", "ok"))},
		scriptPoll{body: hist(false, ev(6, "assistant_message", "Voici le retour final."), ev(7, "turn_ended", ""))},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := c.WaitForReply(ctx, "app", "sess", 0)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if msg.Content != "Voici le retour final." || msg.Seq != 6 {
		t.Fatalf("must return the final answer, got %+v", msg)
	}
}

// A transient 503 before the answer is retried, then the final answer returns.
func TestWaitReply_TransientThenFinal(t *testing.T) {
	c := waitClient(t,
		scriptPoll{status: 503, body: map[string]any{"code": "unavailable", "error": "later"}},
		scriptPoll{body: hist(false, ev(4, "assistant_message", "Fini."), ev(5, "turn_ended", ""))},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := c.WaitForReply(ctx, "app", "sess", 0)
	if err != nil || msg.Content != "Fini." {
		t.Fatalf("want recovered final 'Fini.', got %+v err=%v", msg, err)
	}
}

// A permanent 400 aborts WaitForReply with the API error.
func TestWaitReply_PermanentAborts(t *testing.T) {
	c := waitClient(t,
		scriptPoll{status: 400, body: map[string]any{"code": "bad_request", "error": "nope"}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.WaitForReply(ctx, "app", "sess", 0); err == nil {
		t.Fatal("permanent 400 must abort WaitForReply with an error")
	}
}

// A tool-only turn that ends with no assistant text is an honest no-reply.
func TestWaitReply_ToolOnlyTurnTimesOut(t *testing.T) {
	c := waitClient(t,
		scriptPoll{body: hist(false, toolEv(4, "filesystem.write", "ok"), ev(5, "turn_ended", ""))},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.WaitForReply(ctx, "app", "sess", 0)
	var to *ErrReplyTimeout
	if err == nil || !errorsAs(err, &to) {
		t.Fatalf("want ErrReplyTimeout for a text-less turn, got %v", err)
	}
}
