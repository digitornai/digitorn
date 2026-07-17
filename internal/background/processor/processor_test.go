package processor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
	"github.com/digitornai/digitorn/internal/background/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "bg.db") + "?_pragma=busy_timeout(5000)"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, _ := gdb.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	st := store.New(gdb)
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

type fakeDaemon struct {
	mu       sync.Mutex
	creates  int
	lastBody map[string]json.RawMessage
}

func (f *fakeDaemon) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/history"):
			// Model the real daemon: turn_active stays false; the durable turn_ended
			// event marks completion and assistant text rides an assistant_message event.
			writeJSON(w, 200, map[string]any{
				"turn_active": false,
				"messages": []map[string]any{
					{"seq": 2, "role": "user", "content": "q"},
					{"seq": 5, "role": "assistant", "content": "hi bob"},
				},
				"events": []map[string]any{
					{"seq": 3, "type": "turn_started"},
					{"seq": 5, "type": "assistant_message", "payload": map[string]any{"content": "hi bob"}},
					{"seq": 6, "type": "turn_ended"},
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions"):
			f.creates++
			raw, _ := io.ReadAll(r.Body)
			m := map[string]json.RawMessage{}
			_ = json.Unmarshal(raw, &m)
			f.lastBody = m
			writeJSON(w, 200, map[string]any{"session_id": "x", "first_message": map[string]any{"seq": 2}})
		case r.Method == http.MethodGet && strings.Contains(p, "/sessions/"):
			writeJSON(w, 404, map[string]any{"code": "not_found"})
		default:
			writeJSON(w, 404, map[string]any{})
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type fakeAdapter struct {
	mu    sync.Mutex
	sent  []string
	toRef []map[string]any
}

func (a *fakeAdapter) Name() string                              { return "fake" }
func (a *fakeAdapter) Start(context.Context, adapter.Sink) error { return nil }
func (a *fakeAdapter) Send(_ context.Context, ref map[string]any, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, text)
	a.toRef = append(a.toRef, ref)
	return nil
}

func armTrigger(t *testing.T, st *store.Store, spec TriggerSpec) string {
	t.Helper()
	cfg, _ := json.Marshal(spec)
	tr := &store.Trigger{AppID: spec.AppID, Provider: spec.Provider, Adapter: spec.Adapter, ConfigJSON: string(cfg), Enabled: true}
	if err := st.UpsertTrigger(context.Background(), tr); err != nil {
		t.Fatalf("arm: %v", err)
	}
	return tr.ID
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestEndToEnd proves the full chain in-process: an adapter Event is durably
// taken in, claimed, run through the pipeline, invoked on the (fake) daemon, and
// the agent's reply is delivered back out on the originating adapter.
func TestEndToEnd(t *testing.T) {
	st := newStore(t)
	fd := &fakeDaemon{}
	client := daemonclient.New(fd.server(t).URL, "", daemonclient.WithPollInterval(2_000_000)) // 2ms
	ad := &fakeAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)

	trig := armTrigger(t, st, TriggerSpec{
		AppID: "demo", Provider: "wh", Adapter: "fake", DefaultAgent: "main",
		Activation: channels.ActivationConfig{
			Agent:   "greeter",
			Message: "hello {{event.payload.user}}",
			Session: channels.SessionPerEvent,
			Reply:   channels.ReplyAuto,
		},
	})

	// Durable intake (as an adapter would).
	intake := NewIntake(st, "demo", "wh", trig)
	ev := adapter.Event{
		Provider: "wh", Adapter: "fake", DedupKey: "evt-1", Source: "1.2.3.4",
		Payload:  map[string]any{"user": "bob"},
		ReplyRef: map[string]any{"to": "1.2.3.4"},
	}
	if err := intake.Sink()(context.Background(), ev); err != nil {
		t.Fatalf("intake: %v", err)
	}

	// Claim + process.
	jobs, err := st.Claim(context.Background(), 1, 30_000_000_000)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v n=%d", err, len(jobs))
	}
	p := New(st, client, reg, nil, discard())
	if err := p.Process(context.Background(), jobs[0]); err != nil {
		t.Fatalf("process: %v", err)
	}

	// The daemon was invoked with the rendered message + the routed agent + context.
	if fd.creates != 1 {
		t.Fatalf("expected 1 create, got %d", fd.creates)
	}
	var msg, agent string
	_ = json.Unmarshal(fd.lastBody["message"], &msg)
	_ = json.Unmarshal(fd.lastBody["entry_agent"], &agent)
	if msg != "hello bob" {
		t.Fatalf("rendered message = %q", msg)
	}
	if agent != "greeter" {
		t.Fatalf("entry_agent = %q", agent)
	}
	// The reply was delivered back out on the originating adapter.
	if len(ad.sent) != 1 || ad.sent[0] != "hi bob" {
		t.Fatalf("reply not delivered: %v", ad.sent)
	}
}

// TestFilteredEventCompletes proves a filtered event is durably completed with
// NO daemon call.
func TestFilteredEventCompletes(t *testing.T) {
	st := newStore(t)
	fd := &fakeDaemon{}
	client := daemonclient.New(fd.server(t).URL, "")

	trig := armTrigger(t, st, TriggerSpec{
		AppID: "demo", Provider: "wh", Adapter: "fake",
		Activation: channels.ActivationConfig{
			Message: "x",
			Filter:  []channels.FilterCondition{{Field: "event.payload.status", Equals: "open"}},
		},
	})
	intake := NewIntake(st, "demo", "wh", trig)
	_ = intake.Sink()(context.Background(), adapter.Event{
		Provider: "wh", Adapter: "fake", DedupKey: "e2",
		Payload: map[string]any{"status": "closed"}, // fails the filter
	})

	jobs, _ := st.Claim(context.Background(), 1, 30_000_000_000)
	p := New(st, client, nil, nil, discard())
	if err := p.Process(context.Background(), jobs[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	if fd.creates != 0 {
		t.Fatalf("filtered event must not invoke the daemon, got %d creates", fd.creates)
	}
}

// TestIntakeIdempotent proves a redelivery (same DedupKey) is dropped.
func TestIntakeIdempotent(t *testing.T) {
	st := newStore(t)
	trig := armTrigger(t, st, TriggerSpec{AppID: "demo", Provider: "wh", Adapter: "fake",
		Activation: channels.ActivationConfig{Message: "x"}})
	intake := NewIntake(st, "demo", "wh", trig)
	ev := adapter.Event{Provider: "wh", Adapter: "fake", DedupKey: "dup", Payload: map[string]any{"a": 1}}
	for i := 0; i < 3; i++ {
		if err := intake.Sink()(context.Background(), ev); err != nil {
			t.Fatalf("intake %d: %v", i, err)
		}
	}
	c, _ := st.Counts(context.Background())
	if c.Pending != 1 {
		t.Fatalf("redelivery must dedup to 1 job, got %d pending", c.Pending)
	}
}

// TestNoTrigger_Terminal proves a job without a trigger fails terminally.
func TestNoTrigger_Terminal(t *testing.T) {
	st := newStore(t)
	p := New(st, daemonclient.New("http://127.0.0.1:0", ""), nil, nil, discard())
	err := p.Process(context.Background(), store.Job{ID: "j", AppID: "demo", PayloadJSON: `{"provider":"wh"}`})
	if err == nil {
		t.Fatal("job with no trigger must error terminally")
	}
}
