package processor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
	"github.com/digitornai/digitorn/internal/background/store"
)

// scriptedDaemon is a programmable stand-in for the daemon's public API covering
// the WHOLE channel turn: session create, the seq-ordered /history event stream,
// the parked-approval /state read, and POST /approve. Knobs let one test gate the
// turn's completion on an approval, delay it, or run it straight through.
type scriptedDaemon struct {
	mu sync.Mutex

	// approval, when set, is returned by /state until resolved.
	approval map[string]any
	resolved bool
	action   string // captured from POST /approve
	reason   string
	resolves int

	gateOnResolve   bool // turn_ended only emitted once the approval is resolved
	turnEndAfterHit int  // …and only after this many /history polls (delay knob)
	histHits        int

	// streamEvents are the per-turn events (preamble / tool / final) before turn_ended.
	streamEvents []map[string]any
}

func (d *scriptedDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/history"):
			d.mu.Lock()
			d.histHits++
			hits := d.histHits
			ended := (!d.gateOnResolve || d.resolved) && hits >= d.turnEndAfterHit
			evts := append([]map[string]any{{"seq": 3, "type": "turn_started"}}, d.streamEvents...)
			if ended {
				evts = append(evts, map[string]any{"seq": 90, "type": "turn_ended"})
			}
			d.mu.Unlock()
			writeJSON(w, 200, map[string]any{"turn_active": false, "events": evts,
				"messages": []map[string]any{{"seq": 2, "role": "user", "content": "q"}}})

		case r.Method == http.MethodGet && strings.HasSuffix(p, "/state"):
			d.mu.Lock()
			aps := map[string]any{}
			if d.approval != nil && !d.resolved {
				aps["ap1"] = d.approval
			}
			d.mu.Unlock()
			writeJSON(w, 200, map[string]any{"approvals": aps})

		case r.Method == http.MethodPost && strings.HasSuffix(p, "/approve"):
			raw, _ := io.ReadAll(r.Body)
			m := map[string]any{}
			_ = json.Unmarshal(raw, &m)
			d.mu.Lock()
			d.resolved = true
			d.resolves++
			d.action, _ = m["action"].(string)
			d.reason, _ = m["reason"].(string)
			d.mu.Unlock()
			writeJSON(w, 200, map[string]any{"ok": true})

		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions"):
			raw, _ := io.ReadAll(r.Body)
			m := map[string]json.RawMessage{}
			_ = json.Unmarshal(raw, &m)
			var sid string
			_ = json.Unmarshal(m["session_id"], &sid)
			if sid == "" {
				sid = "sess"
			}
			writeJSON(w, 200, map[string]any{"session_id": sid, "first_message": map[string]any{"seq": 2}})

		case r.Method == http.MethodGet && strings.Contains(p, "/sessions/"):
			writeJSON(w, 404, map[string]any{"code": "not_found"}) // forces create

		default:
			writeJSON(w, 404, map[string]any{"code": "not_found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// prompterAdapter implements Adapter + Typer + Prompter, recording every call so a
// test can assert what reached the channel and feed a scripted prompt answer.
type prompterAdapter struct {
	mu        sync.Mutex
	sent      []string
	toRef     []map[string]any
	typed     int
	prompts   []adapter.PromptRequest
	answer    adapter.PromptResponse
	promptErr error
}

func (a *prompterAdapter) Name() string                              { return "fake" }
func (a *prompterAdapter) Start(context.Context, adapter.Sink) error { return nil }
func (a *prompterAdapter) Send(_ context.Context, ref map[string]any, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, text)
	a.toRef = append(a.toRef, ref)
	return nil
}
func (a *prompterAdapter) Typing(context.Context, map[string]any) error {
	a.mu.Lock()
	a.typed++
	a.mu.Unlock()
	return nil
}
func (a *prompterAdapter) Prompt(_ context.Context, req adapter.PromptRequest) (adapter.PromptResponse, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, req)
	ans, err := a.answer, a.promptErr
	a.mu.Unlock()
	return ans, err
}

func (a *prompterAdapter) snapshot() ([]string, int, []adapter.PromptRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.sent...), a.typed, append([]adapter.PromptRequest(nil), a.prompts...)
}

func runJob(t *testing.T, p *ChannelProcessor, st *store.Store, ev adapter.Event, spec TriggerSpec) {
	t.Helper()
	trig := armTrigger(t, st, spec)
	intake := NewIntake(st, spec.AppID, spec.Provider, trig)
	if err := intake.Sink()(context.Background(), ev); err != nil {
		t.Fatalf("intake: %v", err)
	}
	jobs, err := st.Claim(context.Background(), 10, 30_000_000_000)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("claim: %v n=%d", err, len(jobs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, j := range jobs {
		if err := p.Process(ctx, j); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
}

// TestIntegration_StreamLoopReachesChannel: a reply:stream turn relays the WHOLE
// agentic loop — preamble, a tool line, the final answer — to the originating
// channel, in order, and shows a typing hint while it runs.
func TestIntegration_StreamLoopReachesChannel(t *testing.T) {
	st := newStore(t)
	d := &scriptedDaemon{streamEvents: []map[string]any{
		{"seq": 4, "type": "assistant_message", "payload": map[string]any{"content": "Je regarde…"}},
		{"seq": 5, "type": "tool_result", "payload": map[string]any{"name": "filesystem.glob", "status": "ok"}},
		{"seq": 6, "type": "assistant_message", "payload": map[string]any{"content": "Voici le retour."}},
	}}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 20 * time.Millisecond

	runJob(t, p, st, adapter.Event{
		Provider: "tg", Adapter: "fake", DedupKey: "e1", Source: "chat1",
		ReplyRef: map[string]any{"chat": "chat1"},
	}, TriggerSpec{
		AppID: "demo", Provider: "tg", Adapter: "fake",
		Activation: channels.ActivationConfig{Agent: "main", Message: "analyse",
			Session: channels.SessionPerEvent, Reply: channels.ReplyStream},
	})

	sent, typed, _ := ad.snapshot()
	if len(sent) != 3 {
		t.Fatalf("the channel must see preamble + tool + final (3 items), got %d: %v", len(sent), sent)
	}
	if sent[0] != "Je regarde…" {
		t.Errorf("item 0 (preamble): got %q", sent[0])
	}
	if !strings.Contains(sent[1], "filesystem.glob") {
		t.Errorf("item 1 should be the tool line for filesystem.glob: got %q", sent[1])
	}
	if sent[2] != "Voici le retour." {
		t.Errorf("item 2 (final): got %q", sent[2])
	}
	if typed == 0 {
		t.Error("expected a typing hint during the turn")
	}
}

// TestIntegration_ApprovalSurfacedAndResolved: a reply:auto turn parks on a gated
// tool; the approval is surfaced as a channel prompt, the user's grant is resolved
// back to the daemon, and only THEN does the turn finish and deliver its answer.
func TestIntegration_ApprovalSurfacedAndResolved(t *testing.T) {
	st := newStore(t)
	d := &scriptedDaemon{
		gateOnResolve: true,
		approval: map[string]any{
			"id": "ap1", "kind": "tool_call", "status": "pending",
			"tool_name": "filesystem.write", "risk_level": "medium",
			"reason":      "Writing changes the workspace.",
			"tool_params": map[string]any{"path": "/w/bot.py", "content": "print(1)"},
		},
		streamEvents: []map[string]any{
			{"seq": 6, "type": "assistant_message", "payload": map[string]any{"content": "Fichier écrit."}},
		},
	}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{answer: adapter.PromptResponse{OptionID: "grant", UserID: "u1"}}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 15 * time.Millisecond

	runJob(t, p, st, adapter.Event{
		Provider: "tg", Adapter: "fake", DedupKey: "e2", Source: "chat1",
		ReplyRef: map[string]any{"chat": "chat1"},
	}, TriggerSpec{
		AppID: "demo", Provider: "tg", Adapter: "fake",
		Activation: channels.ActivationConfig{Agent: "main", Message: "écris bot.py",
			Session: channels.SessionPerEvent, Reply: channels.ReplyAuto},
	})

	sent, _, prompts := ad.snapshot()
	if len(prompts) == 0 {
		t.Fatal("the parked approval was never surfaced as a channel prompt")
	}
	if !strings.Contains(prompts[0].Body, "filesystem.write") {
		t.Errorf("prompt body should name the tool: %q", prompts[0].Body)
	}
	d.mu.Lock()
	act, n := d.action, d.resolves
	d.mu.Unlock()
	if act != "grant" || n != 1 {
		t.Fatalf("approval should resolve exactly once with grant, got action=%q resolves=%d", act, n)
	}
	if len(sent) == 0 || sent[len(sent)-1] != "Fichier écrit." {
		t.Fatalf("final answer not delivered after approval: %v", sent)
	}
}

// TestIntegration_PromptErrorDoesNotWedge: when the channel prompt fails, the
// approval is simply left unresolved (still answerable via web/CLI) and the turn
// is NOT crashed or wedged — graceful degradation.
func TestIntegration_PromptErrorDoesNotWedge(t *testing.T) {
	st := newStore(t)
	d := &scriptedDaemon{
		turnEndAfterHit: 25, // keep the turn alive long enough for the pump to fire once
		approval: map[string]any{
			"id": "ap1", "kind": "tool_call", "status": "pending", "tool_name": "filesystem.write",
		},
		streamEvents: []map[string]any{
			{"seq": 6, "type": "assistant_message", "payload": map[string]any{"content": "Terminé."}},
		},
	}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{promptErr: io.ErrUnexpectedEOF}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 15 * time.Millisecond

	runJob(t, p, st, adapter.Event{
		Provider: "tg", Adapter: "fake", DedupKey: "e3", Source: "chat1",
		ReplyRef: map[string]any{"chat": "chat1"},
	}, TriggerSpec{
		AppID: "demo", Provider: "tg", Adapter: "fake",
		Activation: channels.ActivationConfig{Agent: "main", Message: "écris",
			Session: channels.SessionPerEvent, Reply: channels.ReplyAuto},
	})

	d.mu.Lock()
	resolves := d.resolves
	d.mu.Unlock()
	if resolves != 0 {
		t.Fatalf("a failed prompt must NOT resolve the approval, got resolves=%d", resolves)
	}
	sent, _, _ := ad.snapshot()
	if len(sent) == 0 || sent[len(sent)-1] != "Terminé." {
		t.Fatalf("turn must still complete + deliver despite the prompt error: %v", sent)
	}
}

// TestIntegration_ConcurrentSessionsIsolated: two events processed concurrently
// each get exactly their own reply on their own channel ref — no cross-delivery,
// no double-send, no drop, no race (run under -race).
func TestIntegration_ConcurrentSessionsIsolated(t *testing.T) {
	st := newStore(t)
	d := &scriptedDaemon{streamEvents: []map[string]any{
		{"seq": 6, "type": "assistant_message", "payload": map[string]any{"content": "ok"}},
	}}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 20 * time.Millisecond

	// Intake 4 events under one trigger SERIALLY (a real adapter does this on its
	// own goroutine), then claim them all and process CONCURRENTLY — the path that
	// must stay race-free and never cross-deliver.
	trig := armTrigger(t, st, TriggerSpec{AppID: "demo", Provider: "tg", Adapter: "fake",
		Activation: channels.ActivationConfig{Agent: "main", Message: "x",
			Session: channels.SessionPerEvent, Reply: channels.ReplyStream}})
	intake := NewIntake(st, "demo", "tg", trig)
	for _, id := range []string{"A", "B", "C", "D"} {
		if err := intake.Sink()(context.Background(), adapter.Event{
			Provider: "tg", Adapter: "fake", DedupKey: id, Source: id,
			ReplyRef: map[string]any{"chat": id},
		}); err != nil {
			t.Fatalf("intake %s: %v", id, err)
		}
	}
	jobs, err := st.Claim(context.Background(), 10, 30_000_000_000)
	if err != nil || len(jobs) != 4 {
		t.Fatalf("claim: %v n=%d (want 4)", err, len(jobs))
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j store.Job) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := p.Process(ctx, j); err != nil {
				t.Errorf("process %s: %v", j.ID, err)
			}
		}(j)
	}
	wg.Wait()

	sent, _, _ := ad.snapshot()
	if len(sent) != 4 {
		t.Fatalf("each of 4 concurrent turns must deliver exactly one reply, got %d (%v)", len(sent), sent)
	}
	for _, s := range sent {
		if s != "ok" {
			t.Errorf("unexpected reply content under concurrency: %q", s)
		}
	}
}
