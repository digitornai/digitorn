package processor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
	"github.com/mbathepaul/digitorn/internal/background/channels"
	"github.com/mbathepaul/digitorn/internal/background/daemonclient"
	"github.com/mbathepaul/digitorn/internal/background/runner"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Group A — inbound attachments / MediaFetcher (vision path)
// ─────────────────────────────────────────────────────────────────────────────

type mediaResult struct {
	data []byte
	mime string
	err  error
}

// mediaAdapter implements Adapter + MediaFetcher, returning a scripted result per
// attachment filename so a test can drive fetch success/failure/empty.
type mediaAdapter struct {
	files map[string]mediaResult
}

func (m *mediaAdapter) Name() string                                       { return "fake" }
func (m *mediaAdapter) Start(context.Context, adapter.Sink) error          { return nil }
func (m *mediaAdapter) Send(context.Context, map[string]any, string) error { return nil }
func (m *mediaAdapter) FetchMedia(_ context.Context, att adapter.Attachment) ([]byte, string, error) {
	r := m.files[att.Filename]
	return r.data, r.mime, r.err
}

func blobServer(t *testing.T, status int, calls *int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/blobs") {
			writeJSON(w, 404, map[string]any{"code": "not_found"})
			return
		}
		mu.Lock()
		*calls++
		mu.Unlock()
		if status == 0 {
			status = 200
		}
		if status != 200 {
			writeJSON(w, status, map[string]any{"code": "boom", "error": "upload failed"})
			return
		}
		b, _ := io.ReadAll(r.Body)
		writeJSON(w, 200, map[string]any{"hash": "h", "mime": r.Header.Get("Content-Type"), "size": len(b)})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Partial-failure resilience: of three inbound attachments — one good, one whose
// fetch errors, one that returns zero bytes — only the good one becomes a BlobRef,
// and the turn is never aborted.
func TestAttachments_PartialFailureSkipsBadOnes(t *testing.T) {
	calls := 0
	client := daemonclient.New(blobServer(t, 200, &calls).URL, "")
	reg := adapter.NewRegistry()
	reg.Register(&mediaAdapter{files: map[string]mediaResult{
		"ok.png":    {data: []byte{1, 2, 3}, mime: "image/png"},
		"boom.png":  {err: io.ErrUnexpectedEOF},
		"empty.png": {data: nil, mime: "image/png"},
	}})
	p := New(nil, client, reg, nil, discard())

	ev := adapter.Event{Adapter: "fake", Attachments: []adapter.Attachment{
		{Filename: "ok.png"}, {Filename: "boom.png"}, {Filename: "empty.png"},
	}}
	refs := p.resolveAttachments(context.Background(), ev, "demo")
	if len(refs) != 1 {
		t.Fatalf("only the good attachment should yield a ref, got %d", len(refs))
	}
	if calls != 1 {
		t.Fatalf("only the good attachment should hit the blob store, got %d uploads", calls)
	}
}

// An upload failure (daemon blob store 500) is swallowed per file — no ref, no panic,
// no turn abort.
func TestAttachments_UploadFailureSkips(t *testing.T) {
	calls := 0
	client := daemonclient.New(blobServer(t, 500, &calls).URL, "")
	reg := adapter.NewRegistry()
	reg.Register(&mediaAdapter{files: map[string]mediaResult{"ok.png": {data: []byte{1}, mime: "image/png"}}})
	p := New(nil, client, reg, nil, discard())

	refs := p.resolveAttachments(context.Background(),
		adapter.Event{Adapter: "fake", Attachments: []adapter.Attachment{{Filename: "ok.png"}}}, "demo")
	if len(refs) != 0 {
		t.Fatalf("upload failure must yield no refs, got %d", len(refs))
	}
}

// An adapter that is not a MediaFetcher simply contributes no attachments.
func TestAttachments_NonFetcherAdapterNoOp(t *testing.T) {
	reg := adapter.NewRegistry()
	reg.Register(&prompterAdapter{}) // not a MediaFetcher
	p := New(nil, daemonclient.New("http://127.0.0.1:0", ""), reg, nil, discard())
	refs := p.resolveAttachments(context.Background(),
		adapter.Event{Adapter: "fake", Attachments: []adapter.Attachment{{Filename: "x"}}}, "demo")
	if refs != nil {
		t.Fatalf("a non-fetcher adapter must yield nil refs, got %v", refs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Group B — approval pump: parallel surfacing, dedup, ask_user question
// ─────────────────────────────────────────────────────────────────────────────

type apvDaemon struct {
	mu              sync.Mutex
	approvals       map[string]map[string]any
	sticky          bool // keep returning approvals on /state even after they resolve
	resolved        map[string]bool
	resolveCalls    int
	lastReason      map[string]string
	turnEndAfterHit int
	histHits        int
	final           string
}

func (d *apvDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	d.resolved = map[string]bool{}
	d.lastReason = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/history"):
			d.mu.Lock()
			d.histHits++
			ended := d.histHits >= d.turnEndAfterHit
			d.mu.Unlock()
			evts := []map[string]any{{"seq": 3, "type": "turn_started"}}
			if d.final != "" {
				evts = append(evts, map[string]any{"seq": 5, "type": "assistant_message", "payload": map[string]any{"content": d.final}})
			}
			if ended {
				evts = append(evts, map[string]any{"seq": 9, "type": "turn_ended"})
			}
			writeJSON(w, 200, map[string]any{"turn_active": false, "events": evts})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/state"):
			d.mu.Lock()
			aps := map[string]any{}
			for id, body := range d.approvals {
				if d.sticky || !d.resolved[id] {
					aps[id] = body
				}
			}
			d.mu.Unlock()
			writeJSON(w, 200, map[string]any{"approvals": aps})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/approve"):
			raw, _ := io.ReadAll(r.Body)
			m := map[string]any{}
			_ = json.Unmarshal(raw, &m)
			id, _ := m["approval_id"].(string)
			rs, _ := m["reason"].(string)
			d.mu.Lock()
			d.resolved[id] = true
			d.resolveCalls++
			d.lastReason[id] = rs
			d.mu.Unlock()
			writeJSON(w, 200, map[string]any{"ok": true})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions"):
			writeJSON(w, 200, map[string]any{"session_id": "sess", "first_message": map[string]any{"seq": 2}})
		case r.Method == http.MethodGet && strings.Contains(p, "/sessions/"):
			writeJSON(w, 404, map[string]any{"code": "not_found"})
		default:
			writeJSON(w, 404, map[string]any{"code": "not_found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Two distinct approvals parked at once must BOTH be surfaced (concurrently) and
// both resolved — parallel tool approvals don't serialize behind one another.
func TestApprovalPump_ParallelBothSurfaced(t *testing.T) {
	st := newStore(t)
	d := &apvDaemon{
		turnEndAfterHit: 40, final: "done",
		approvals: map[string]map[string]any{
			"ap1": {"id": "ap1", "kind": "tool_call", "status": "pending", "tool_name": "filesystem.write"},
			"ap2": {"id": "ap2", "kind": "tool_call", "status": "pending", "tool_name": "filesystem.delete"},
		},
	}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{answer: adapter.PromptResponse{OptionID: "grant"}}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 10 * time.Millisecond

	runJob(t, p, st, adapter.Event{Provider: "tg", Adapter: "fake", DedupKey: "e1", Source: "c1",
		ReplyRef: map[string]any{"chat": "c1"}},
		TriggerSpec{AppID: "demo", Provider: "tg", Adapter: "fake",
			Activation: channels.ActivationConfig{Agent: "main", Message: "x",
				Session: channels.SessionPerEvent, Reply: channels.ReplyAuto}})

	_, _, prompts := ad.snapshot()
	if len(prompts) != 2 {
		t.Fatalf("both parked approvals must be surfaced, got %d prompts", len(prompts))
	}
	d.mu.Lock()
	n := d.resolveCalls
	d.mu.Unlock()
	if n != 2 {
		t.Fatalf("both approvals must resolve, got %d resolves", n)
	}
}

// The same approval id returned on EVERY /state poll must be surfaced exactly once
// — the pump's handled-set dedups it, no matter how many times it reappears.
func TestApprovalPump_DedupRepeatedID(t *testing.T) {
	st := newStore(t)
	d := &apvDaemon{
		turnEndAfterHit: 80, final: "done", sticky: true, // ap1 keeps coming back
		approvals: map[string]map[string]any{
			"ap1": {"id": "ap1", "kind": "tool_call", "status": "pending", "tool_name": "filesystem.write"},
		},
	}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{answer: adapter.PromptResponse{OptionID: "grant"}}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 8 * time.Millisecond

	runJob(t, p, st, adapter.Event{Provider: "tg", Adapter: "fake", DedupKey: "e2", Source: "c1",
		ReplyRef: map[string]any{"chat": "c1"}},
		TriggerSpec{AppID: "demo", Provider: "tg", Adapter: "fake",
			Activation: channels.ActivationConfig{Agent: "main", Message: "x",
				Session: channels.SessionPerEvent, Reply: channels.ReplyAuto}})

	if _, _, prompts := ad.snapshot(); len(prompts) != 1 {
		t.Fatalf("a sticky approval must be surfaced exactly once, got %d prompts", len(prompts))
	}
}

// An ask_user question (choices) is surfaced as a prompt and the user's chosen
// option is resolved back as the answer text.
func TestApprovalPump_AskUserQuestionResolved(t *testing.T) {
	st := newStore(t)
	d := &apvDaemon{
		turnEndAfterHit: 40, final: "merci",
		approvals: map[string]map[string]any{
			"q1": {"id": "q1", "kind": "question", "status": "pending",
				"reason": "Quelle couleur ?", "payload": map[string]any{"choices": []any{"Rouge", "Bleu"}}},
		},
	}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{answer: adapter.PromptResponse{OptionID: "Bleu", UserID: "u1"}}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 10 * time.Millisecond

	runJob(t, p, st, adapter.Event{Provider: "tg", Adapter: "fake", DedupKey: "e3", Source: "c1",
		ReplyRef: map[string]any{"chat": "c1"}},
		TriggerSpec{AppID: "demo", Provider: "tg", Adapter: "fake",
			Activation: channels.ActivationConfig{Agent: "main", Message: "x",
				Session: channels.SessionPerEvent, Reply: channels.ReplyAuto}})

	_, _, prompts := ad.snapshot()
	if len(prompts) != 1 || !strings.Contains(prompts[0].Body, "Quelle couleur") {
		t.Fatalf("question not surfaced with its text: %+v", prompts)
	}
	d.mu.Lock()
	reason := d.lastReason["q1"]
	d.mu.Unlock()
	if reason != "Bleu" {
		t.Fatalf("the chosen option must be resolved as the answer, got %q", reason)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Group C — Process-level fault injection (retry classification, idempotency)
// ─────────────────────────────────────────────────────────────────────────────

func faultServer(t *testing.T, createStatus int, sessionExists bool, creates *int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/history"):
			writeJSON(w, 200, map[string]any{"turn_active": false,
				"events": []map[string]any{{"seq": 3, "type": "turn_started"},
					{"seq": 5, "type": "assistant_message", "payload": map[string]any{"content": "ok"}},
					{"seq": 6, "type": "turn_ended"}}})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions"):
			mu.Lock()
			*creates++
			mu.Unlock()
			if createStatus != 0 && createStatus != 200 {
				writeJSON(w, createStatus, map[string]any{"code": "x", "error": "create failed"})
				return
			}
			writeJSON(w, 200, map[string]any{"session_id": "sess", "first_message": map[string]any{"seq": 2}})
		case r.Method == http.MethodGet && strings.Contains(p, "/sessions/"):
			if sessionExists {
				writeJSON(w, 200, map[string]any{"session_id": "sess"})
			} else {
				writeJSON(w, 404, map[string]any{"code": "not_found"})
			}
		default:
			writeJSON(w, 404, map[string]any{"code": "not_found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func processOne(t *testing.T, p *ChannelProcessor, st *store.Store, reply string) error {
	t.Helper()
	trig := armTrigger(t, st, TriggerSpec{AppID: "demo", Provider: "wh", Adapter: "fake",
		Activation: channels.ActivationConfig{Agent: "main", Message: "x",
			Session: channels.SessionPerEvent, Reply: reply}})
	intake := NewIntake(st, "demo", "wh", trig)
	if err := intake.Sink()(context.Background(), adapter.Event{Provider: "wh", Adapter: "fake", DedupKey: "f1"}); err != nil {
		t.Fatalf("intake: %v", err)
	}
	jobs, err := st.Claim(context.Background(), 1, 30_000_000_000)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v n=%d", err, len(jobs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Process(ctx, jobs[0])
}

// A 5xx on session create is a TRANSIENT fault: Process returns a runner.Retryable
// so the pool re-queues the job instead of dropping the event.
func TestProcess_Create5xxIsRetryable(t *testing.T) {
	st := newStore(t)
	creates := 0
	client := daemonclient.New(faultServer(t, 503, false, &creates).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	p := New(st, client, adapter.NewRegistry(), nil, discard())

	err := processOne(t, p, st, channels.ReplyNone)
	var rt *runner.Retryable
	if err == nil || !errors.As(err, &rt) {
		t.Fatalf("a 5xx create must be retryable, got %v", err)
	}
}

// A 4xx on session create is a PERMANENT fault: Process returns a terminal error
// (NOT retryable) so a broken request fails fast instead of looping forever.
func TestProcess_Create4xxIsTerminal(t *testing.T) {
	st := newStore(t)
	creates := 0
	client := daemonclient.New(faultServer(t, 400, false, &creates).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	p := New(st, client, adapter.NewRegistry(), nil, discard())

	err := processOne(t, p, st, channels.ReplyNone)
	var rt *runner.Retryable
	if err == nil {
		t.Fatal("a 4xx create must fail")
	}
	if errors.As(err, &rt) {
		t.Fatalf("a 4xx create must be TERMINAL, not retryable: %v", err)
	}
}

// A retry whose per-job session already exists must NOT create a second session
// (idempotency): Launch sees the existing id and no-ops the create.
func TestProcess_IdempotentRetryNoDoubleCreate(t *testing.T) {
	st := newStore(t)
	creates := 0
	client := daemonclient.New(faultServer(t, 200, true /*exists*/, &creates).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	p := New(st, client, adapter.NewRegistry(), nil, discard())

	if err := processOne(t, p, st, channels.ReplyNone); err != nil {
		t.Fatalf("idempotent retry should succeed, got %v", err)
	}
	if creates != 0 {
		t.Fatalf("an already-existing per-job session must not be re-created, got %d creates", creates)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Group D — approval pump lifecycle: stop() cancels in-flight prompts, no leak
// ─────────────────────────────────────────────────────────────────────────────

// blockingPrompter blocks inside Prompt until its context is cancelled, so a test
// can verify the pump's stop() propagates cancellation and joins the goroutine.
type blockingPrompter struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingPrompter) Name() string                              { return "fake" }
func (b *blockingPrompter) Start(context.Context, adapter.Sink) error { return nil }
func (b *blockingPrompter) Send(context.Context, map[string]any, string) error {
	return nil
}
func (b *blockingPrompter) Prompt(ctx context.Context, _ adapter.PromptRequest) (adapter.PromptResponse, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done() // a user who never clicks — held until the pump is stopped
	return adapter.PromptResponse{}, ctx.Err()
}

// When a turn ends while an approval prompt is still open in the channel, the pump's
// stop() must cancel that in-flight prompt and join its goroutine promptly — never
// hang, never leak. (A stuck user is the common case: the turn timed out server-side
// but the buttons are still on screen.)
func TestApprovalPump_StopCancelsInflightPrompt(t *testing.T) {
	d := &apvDaemon{sticky: true, approvals: map[string]map[string]any{
		"ap1": {"id": "ap1", "kind": "tool_call", "status": "pending", "tool_name": "filesystem.write"},
	}}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	bp := &blockingPrompter{entered: make(chan struct{})}
	reg := adapter.NewRegistry()
	reg.Register(bp)
	p := New(nil, client, reg, nil, discard())
	p.approvalPoll = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := p.pumpApprovals(ctx, adapter.Event{Adapter: "fake", ReplyRef: map[string]any{"chat": "c"}}, "demo", "sess")
	if stop == nil {
		t.Fatal("pump must be active when a Prompter is present and ReplyRef is set")
	}

	select {
	case <-bp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval prompt was never surfaced")
	}

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop() hung — the pump leaked an in-flight-prompt goroutine")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Group E — outbound secret scrubbing on delivery
// ─────────────────────────────────────────────────────────────────────────────

// A reply that accidentally contains a secret (an API key the agent echoed) must be
// scrubbed before it leaves on the channel when secret_filter is enabled.
func TestDeliverReply_SecretFilterScrubs(t *testing.T) {
	ad := &prompterAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(nil, nil, reg, nil, discard())

	leak := "Voici la clé : sk-abcdEFGH1234567890wxyz et c'est tout."
	p.deliverReply(context.Background(),
		adapter.Event{Adapter: "fake", ReplyRef: map[string]any{"to": "x"}},
		channels.Activation{}, TriggerSpec{SecretFilter: true}, leak)

	sent, _, _ := ad.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected one delivery, got %d", len(sent))
	}
	if strings.Contains(sent[0], "sk-abcdEFGH1234567890wxyz") {
		t.Fatalf("secret was NOT scrubbed before delivery: %q", sent[0])
	}
	if !strings.Contains(sent[0], "***") {
		t.Errorf("expected a redaction marker in the delivered text: %q", sent[0])
	}
}

// With secret_filter OFF, the text is delivered verbatim (opt-in behaviour).
func TestDeliverReply_NoSecretFilterPassthrough(t *testing.T) {
	ad := &prompterAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(nil, nil, reg, nil, discard())

	text := "clé sk-abcdEFGH1234567890wxyz"
	p.deliverReply(context.Background(),
		adapter.Event{Adapter: "fake", ReplyRef: map[string]any{"to": "x"}},
		channels.Activation{}, TriggerSpec{SecretFilter: false}, text)

	if sent, _, _ := ad.snapshot(); len(sent) != 1 || sent[0] != text {
		t.Fatalf("filter off must pass text verbatim, got %v", sent)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Group F — execution report recorded for every attempt (bg_runs)
// ─────────────────────────────────────────────────────────────────────────────

// A successful reply:auto turn leaves a run report with outcome ok, the produced
// session id, and a reply excerpt — the data the ops surface shows.
func TestProcess_RecordsRunReport_Success(t *testing.T) {
	st := newStore(t)
	d := &scriptedDaemon{streamEvents: []map[string]any{
		{"seq": 6, "type": "assistant_message", "payload": map[string]any{"content": "bonjour le monde"}},
	}}
	client := daemonclient.New(d.server(t).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	ad := &prompterAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(st, client, reg, nil, discard())
	p.approvalPoll = 20 * time.Millisecond

	runJob(t, p, st, adapter.Event{Provider: "tg", Adapter: "fake", DedupKey: "rep1", Source: "c1",
		ReplyRef: map[string]any{"chat": "c1"}},
		TriggerSpec{AppID: "demo", Provider: "tg", Adapter: "fake",
			Activation: channels.ActivationConfig{Agent: "main", Message: "salut",
				Session: channels.SessionPerEvent, Reply: channels.ReplyAuto}})

	runs, err := st.ListRuns(context.Background(), store.RunFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one run report, got %d err=%v", len(runs), err)
	}
	r := runs[0]
	if r.Outcome != "ok" {
		t.Errorf("outcome = %q, want ok", r.Outcome)
	}
	if !strings.HasPrefix(r.SessionID, "bg-") {
		t.Errorf("produced session id not captured: %q", r.SessionID)
	}
	if !strings.Contains(r.ReplyPreview, "bonjour le monde") || r.ReplyChars == 0 {
		t.Errorf("reply excerpt missing: preview=%q chars=%d", r.ReplyPreview, r.ReplyChars)
	}
	if r.DurationMs < 0 || r.EndedAt.Before(r.StartedAt) {
		t.Errorf("bad timing: start=%v end=%v dur=%d", r.StartedAt, r.EndedAt, r.DurationMs)
	}
}

// A failed launch leaves a run report with outcome failed + the error text, so an
// operator can see WHY a trigger fired but produced nothing.
func TestProcess_RecordsRunReport_Failure(t *testing.T) {
	st := newStore(t)
	creates := 0
	client := daemonclient.New(faultServer(t, 400, false, &creates).URL, "", daemonclient.WithPollInterval(2*time.Millisecond))
	p := New(st, client, adapter.NewRegistry(), nil, discard())

	if err := processOne(t, p, st, channels.ReplyNone); err == nil {
		t.Fatal("expected the 400 create to fail the job")
	}
	runs, err := st.ListRuns(context.Background(), store.RunFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one run report, got %d err=%v", len(runs), err)
	}
	if runs[0].Outcome != "failed" || runs[0].Error == "" {
		t.Fatalf("failed run report missing outcome/error: %+v", runs[0])
	}
}
