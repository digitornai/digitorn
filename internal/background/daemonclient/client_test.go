package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/runner"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// fakeDaemon is a minimal stand-in for the daemon's public HTTP API. It records
// what the client did and is programmable per-test (status overrides, history).
type fakeDaemon struct {
	mu sync.Mutex

	creates  int
	posts    int
	exists   map[string]bool // sid -> created
	lastAuth string
	lastBody map[string]json.RawMessage

	createStatus int // 0 → 200
	postStatus   int // 0 → 201

	// history is served by sid; historyGate, if >0, withholds the assistant reply
	// until the Nth History poll, exercising the polling loop.
	history     map[string][]Message
	historyHits map[string]int
	historyGate int
}

func newFake() *fakeDaemon {
	return &fakeDaemon{
		exists:      map[string]bool{},
		history:     map[string][]Message{},
		historyHits: map[string]int{},
	}
}

func (f *fakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeDaemon) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuth = r.Header.Get("Authorization")
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/history"):
		sid := segBefore(path, "/history")
		f.historyHits[sid]++
		msgs := f.history[sid]
		if f.historyGate > 0 && f.historyHits[sid] < f.historyGate {
			// withhold the assistant reply until the gate poll
			var held []Message
			for _, m := range msgs {
				if m.Role != "assistant" {
					held = append(held, m)
				}
			}
			msgs = held
		}
		writeJSONT(w, 200, map[string]any{"messages": msgs})

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
		f.posts++
		body := readBody(r)
		f.lastBody = body
		writeJSONT(w, statusOr(f.postStatus, 201), map[string]any{"session_id": segBefore(path, "/messages"), "seq": 42, "role": "user"})

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions"):
		f.creates++
		body := readBody(r)
		f.lastBody = body
		var sid string
		_ = json.Unmarshal(body["session_id"], &sid)
		if sid != "" {
			f.exists[sid] = true
		}
		writeJSONT(w, statusOr(f.createStatus, 200), map[string]any{
			"session_id":    sid,
			"seq":           1,
			"first_message": map[string]any{"seq": 2},
		})

	case r.Method == http.MethodGet && strings.Contains(path, "/sessions/"):
		// SessionExists probe
		sid := lastSeg(path)
		if f.exists[sid] {
			writeJSONT(w, 200, map[string]any{"session_id": sid})
		} else {
			writeJSONT(w, 404, map[string]any{"code": "not_found", "error": "no such session"})
		}

	default:
		writeJSONT(w, 404, map[string]any{"code": "not_found"})
	}
}

func readBody(r *http.Request) map[string]json.RawMessage {
	raw, _ := io.ReadAll(r.Body)
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func writeJSONT(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func statusOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func segBefore(path, suffix string) string {
	p := strings.TrimSuffix(path, suffix)
	return lastSeg(p)
}

func lastSeg(p string) string {
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

// ── tests ────────────────────────────────────────────────────────────────

func TestCreateSession_InlineMessage(t *testing.T) {
	f := newFake()
	c := New(f.server(t).URL, "")
	res, err := c.Launch(context.Background(), LaunchSpec{
		AppID:   "demo",
		Message: "hello",
	}, "bg-job1")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !res.Created || res.SessionID != "bg-job1" || res.UserSeq != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if f.creates != 1 || f.posts != 0 {
		t.Fatalf("creates=%d posts=%d, want 1/0", f.creates, f.posts)
	}
	var msg string
	_ = json.Unmarshal(f.lastBody["message"], &msg)
	if msg != "hello" {
		t.Fatalf("inline message not sent, got %q", msg)
	}
}

func TestLaunch_PerJob_IdempotentOnRetry(t *testing.T) {
	f := newFake()
	c := New(f.server(t).URL, "")
	spec := LaunchSpec{AppID: "demo", Message: "hi"}

	r1, err := c.Launch(context.Background(), spec, "bg-jobX")
	if err != nil || !r1.Created {
		t.Fatalf("first launch: %+v err=%v", r1, err)
	}
	// Simulate a lease-expiry retry of the SAME job: the deterministic session id
	// already exists → must be an idempotent no-op, NOT a second session/turn.
	r2, err := c.Launch(context.Background(), spec, "bg-jobX")
	if err != nil {
		t.Fatalf("retry launch: %v", err)
	}
	if !r2.Idempotent || r2.Created {
		t.Fatalf("retry should be idempotent skip, got %+v", r2)
	}
	if f.creates != 1 || f.posts != 0 {
		t.Fatalf("retry caused extra calls: creates=%d posts=%d", f.creates, f.posts)
	}
}

func TestLaunch_SharedSession_PostsMessage(t *testing.T) {
	f := newFake()
	f.exists["room-7"] = true // shared session already provisioned
	c := New(f.server(t).URL, "")
	r, err := c.Launch(context.Background(), LaunchSpec{
		AppID:     "demo",
		SessionID: "room-7",
		Message:   "next event",
	}, "bg-job2")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if r.Created || r.Idempotent || r.SessionID != "room-7" || r.UserSeq != 42 {
		t.Fatalf("unexpected: %+v", r)
	}
	if f.creates != 0 || f.posts != 1 {
		t.Fatalf("shared launch should post once: creates=%d posts=%d", f.creates, f.posts)
	}
}

func TestWaitForReply_PollsUntilAssistant(t *testing.T) {
	f := newFake()
	f.historyGate = 3 // assistant withheld until the 3rd poll
	f.history["bg-w"] = []Message{
		{Seq: 2, Role: "user", Content: "hi"},
		{Seq: 5, Role: "assistant", Content: "the answer"},
	}
	f.exists["bg-w"] = false
	c := New(f.server(t).URL, "", WithPollInterval(2*time.Millisecond))

	res, err := c.Launch(context.Background(), LaunchSpec{
		AppID:        "demo",
		Message:      "q",
		WaitForReply: true,
		ReplyTimeout: 2 * time.Second,
	}, "bg-w")
	if err != nil {
		t.Fatalf("Launch+wait: %v", err)
	}
	if res.Reply != "the answer" || res.ReplySeq != 5 {
		t.Fatalf("reply not captured: %+v", res)
	}
	if f.historyHits["bg-w"] < 3 {
		t.Fatalf("expected polling (>=3 hits), got %d", f.historyHits["bg-w"])
	}
}

func TestWaitForReply_TimesOut_NoResend(t *testing.T) {
	f := newFake()
	f.history["bg-t"] = []Message{{Seq: 2, Role: "user", Content: "q"}} // never an assistant reply
	c := New(f.server(t).URL, "", WithPollInterval(2*time.Millisecond))

	_, err := c.Launch(context.Background(), LaunchSpec{
		AppID:        "demo",
		Message:      "q",
		WaitForReply: true,
		ReplyTimeout: 40 * time.Millisecond,
	}, "bg-t")
	var to *ErrReplyTimeout
	if err == nil || !errorsAs(err, &to) {
		t.Fatalf("want ErrReplyTimeout, got %v", err)
	}
}

func TestAuthHeaderForwarded(t *testing.T) {
	f := newFake()
	c := New(f.server(t).URL, "svc-jwt-123")
	_, _ = c.Launch(context.Background(), LaunchSpec{AppID: "demo", Message: "x"}, "bg-a")
	if f.lastAuth != "Bearer svc-jwt-123" {
		t.Fatalf("auth not forwarded: %q", f.lastAuth)
	}
}

func TestProcessor_RetryVsTerminal(t *testing.T) {
	// 503 on the exists probe → transient → Retryable.
	f := newFake()
	f.createStatus = 0
	// Force exists to 5xx by routing: make the probe hit a server that 500s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONT(w, 503, map[string]any{"code": "unavailable"})
	}))
	t.Cleanup(srv.Close)
	p := NewProcessor(New(srv.URL, ""), slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := p.Process(context.Background(), store.Job{ID: "j1", AppID: "demo", PayloadJSON: `{"message":"hi"}`, Attempts: 1})
	var rt *runner.Retryable
	if !errorsAs(err, &rt) {
		t.Fatalf("503 should be retryable, got %v", err)
	}

	// 400 on create → permanent → terminal (not Retryable).
	f2 := newFake()
	f2.createStatus = 400
	p2 := NewProcessor(New(f2.server(t).URL, ""), slog.New(slog.NewTextHandler(io.Discard, nil)))
	err2 := p2.Process(context.Background(), store.Job{ID: "j2", AppID: "demo", PayloadJSON: `{"message":"hi"}`, Attempts: 1})
	if err2 == nil || errorsAs(err2, &rt) {
		t.Fatalf("400 should be terminal, got %v", err2)
	}
}

func TestProcessor_MalformedPayload_Terminal(t *testing.T) {
	p := NewProcessor(New("http://127.0.0.1:0", ""), slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := p.Process(context.Background(), store.Job{ID: "jbad", AppID: "demo", PayloadJSON: `{not json`})
	var rt *runner.Retryable
	if err == nil || errorsAs(err, &rt) {
		t.Fatalf("malformed payload should be terminal, got %v", err)
	}
}

func TestProcessor_NoMessage_Terminal(t *testing.T) {
	p := NewProcessor(New("http://127.0.0.1:0", ""), slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := p.Process(context.Background(), store.Job{ID: "jnm", AppID: "demo", PayloadJSON: `{}`})
	var rt *runner.Retryable
	if err == nil || errorsAs(err, &rt) {
		t.Fatalf("missing message should be terminal, got %v", err)
	}
}
