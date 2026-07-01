package runtime_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// ---- stubs ---------------------------------------------------------

type stubApps struct {
	app *appmgr.RuntimeApp
	err error
	mu  sync.Mutex
	got string
}

func (s *stubApps) Get(_ context.Context, id string) (*appmgr.RuntimeApp, error) {
	s.mu.Lock()
	s.got = id
	s.mu.Unlock()
	return s.app, s.err
}

type stubSessions struct {
	state     *sessionstore.SessionState
	stateErr  error
	stateGot  string
	appendErr error
	// appendErrOnType, when non-empty, makes AppendDurable return
	// appendErr ONLY for events of this type. Lets tests target a
	// specific lifecycle event (e.g. fail only on
	// EventAssistantMessage) instead of every Turn lifecycle event.
	appendErrOnType sessionstore.EventType
	appendSeq       uint64
	mu              sync.Mutex // guards the append* fields : turns run in
	// their own goroutine while a test goroutine reads via findAppend.
	appendEvents  []sessionstore.Event
	appendCalled  bool
	appendCallCnt int
}

func (s *stubSessions) State(sid string) (*sessionstore.SessionState, error) {
	s.mu.Lock()
	s.stateGot = sid
	s.mu.Unlock()
	return s.state, s.stateErr
}

func (s *stubSessions) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCalled = true
	s.appendCallCnt++
	if s.appendErr != nil {
		if s.appendErrOnType == "" || s.appendErrOnType == ev.Type {
			return 0, s.appendErr
		}
	}
	s.appendEvents = append(s.appendEvents, ev)
	return s.appendSeq, nil
}

func (s *stubSessions) Append(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	return s.AppendDurable(ctx, ev)
}

// findAppend returns a COPY of the first event of the given type, or nil
// if none was appended. Returns a copy (not a pointer into the slice) so
// the caller can read it without holding the lock while turns keep
// appending. Used by tests asserting "event X was / was not emitted".
func (s *stubSessions) findAppend(t sessionstore.EventType) *sessionstore.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.appendEvents {
		if s.appendEvents[i].Type == t {
			ev := s.appendEvents[i]
			return &ev
		}
	}
	return nil
}

func (s *stubSessions) countAppend(t sessionstore.EventType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.appendEvents {
		if s.appendEvents[i].Type == t {
			n++
		}
	}
	return n
}

// collectAppend returns COPIES of every appended event of the given type, under
// the lock — so a test goroutine can scan them while turns keep appending
// concurrently (the slice itself must never be read unsynchronised).
func (s *stubSessions) collectAppend(t sessionstore.EventType) []sessionstore.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sessionstore.Event
	for i := range s.appendEvents {
		if s.appendEvents[i].Type == t {
			out = append(out, s.appendEvents[i])
		}
	}
	return out
}

type stubLLM struct {
	resp  *llm.ChatResponse
	err   error
	got   *llm.ChatRequest
	calls int
	mu    sync.Mutex

	// responses is an optional ordered sequence; when set, each Chat
	// call consumes the next entry. After the last response, returns
	// a synthetic terminal response (no tool_calls) so the agent loop
	// in runPhases breaks. Lets RT-3 tests script multi-round LLM ↔
	// tool exchanges. If both resp and responses are set, responses
	// takes precedence.
	responses []*llm.ChatResponse

	// allGots captures every ChatRequest the engine made, in order.
	// Lets RT-3 tests assert what the LLM saw at iteration K (not just
	// the last call). DeepCopy the messages because the engine may
	// reuse buffers in future optimisations.
	allGots []*llm.ChatRequest

	// onCall is invoked before each Chat returns. Lets tests inject
	// blocking / cancellation / panic behaviour without changing the
	// stub's surface. Nil = no-op.
	onCall func(idx int, req *llm.ChatRequest)
}

func (s *stubLLM) Chat(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	s.mu.Lock()
	s.got = req
	idx := s.calls
	s.calls++
	// Snapshot of messages — guard against later mutation.
	cp := *req
	cp.Messages = append([]llm.ChatMessage(nil), req.Messages...)
	s.allGots = append(s.allGots, &cp)
	cb := s.onCall
	s.mu.Unlock()
	if cb != nil {
		cb(idx, req)
	}
	if len(s.responses) > 0 {
		if idx < len(s.responses) {
			return s.responses[idx], s.err
		}
		return &llm.ChatResponse{Content: "(terminal)", Model: "stub"}, s.err
	}
	// Legacy single-response path : the first call returns resp; any
	// follow-up (agent loop after tool dispatch) returns a terminal
	// no-tool-calls response so RT-2 tests still pass under the loop.
	if idx == 0 {
		return s.resp, s.err
	}
	if s.resp != nil && len(s.resp.ToolCalls) > 0 {
		return &llm.ChatResponse{Content: "(terminal)", Model: s.resp.Model}, s.err
	}
	return s.resp, s.err
}

// callCount returns how many times Chat has been invoked, read under the
// stub's lock so a test goroutine can poll it while a turn goroutine is
// still calling Chat (race-free).
func (s *stubLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ---- helpers -------------------------------------------------------

func okApp(t *testing.T, prompt, systemPrompt string, brain schema.Brain) *appmgr.RuntimeApp {
	t.Helper()
	return okAppBYOK(t, prompt, systemPrompt, brain, false)
}

// okAppBYOK builds a RuntimeApp with the BYOK flag set to byok. Use
// the byok=true variant to exercise the direct-provider routing path
// where the brain credential is read ; byok=false (default) keeps the
// engine on the gateway path.
func okAppBYOK(t *testing.T, prompt, systemPrompt string, brain schema.Brain, byok bool) *appmgr.RuntimeApp {
	t.Helper()
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "app-1", BYOK: byok},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID:           "primary",
				Brain:        brain,
				SystemPrompt: systemPrompt,
				Prompt:       prompt,
			}},
		},
		BundleDir: "/tmp/app-1",
	}
}

func okState(t *testing.T, msgs ...sessionstore.Message) *sessionstore.SessionState {
	t.Helper()
	s := sessionstore.NewSessionState("sess-1")
	s.Messages = append(s.Messages, msgs...)
	return s
}

func okLLM() *stubLLM {
	return &stubLLM{
		resp: &llm.ChatResponse{
			Content: "hello back",
			Model:   "claude-3-5-sonnet",
			Usage:   llm.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
		},
	}
}

func newEngine(t *testing.T, apps runtime.AppLookup, sess runtime.SessionAccess, l runtime.LLMChat) *runtime.Engine {
	t.Helper()
	// Discard logger : a nil logger falls back to slog.Default() which writes
	// synchronously to stderr. Under the 10K-turn stress test that serialises
	// every "turn complete" Info line on the stderr lock and inflates the
	// measured p99 — an I/O artefact, not engine cost. Production wires its
	// own configured sink ; tests must not measure stderr throughput.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e, err := runtime.New(apps, sess, l, logger)
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	return e
}

// ---- New() ---------------------------------------------------------

func TestNew_RejectsNilDeps(t *testing.T) {
	apps := &stubApps{}
	sess := &stubSessions{}
	lc := &stubLLM{}
	cases := []struct {
		name string
		a    runtime.AppLookup
		s    runtime.SessionAccess
		l    runtime.LLMChat
	}{
		{"nil apps", nil, sess, lc},
		{"nil sessions", apps, nil, lc},
		{"nil llm", apps, sess, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := runtime.New(c.a, c.s, c.l, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNew_NilLoggerFallsBackToDefault(t *testing.T) {
	e, err := runtime.New(&stubApps{}, &stubSessions{}, &stubLLM{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Logger == nil {
		t.Error("nil logger should fall back to slog.Default()")
	}
}

// ---- Run() : input validation --------------------------------------

func TestRun_RejectsEmptyIDs(t *testing.T) {
	e := newEngine(t, &stubApps{}, &stubSessions{}, &stubLLM{})
	cases := []runtime.TurnInput{
		{AppID: "", SessionID: "sess-1"},
		{AppID: "app-1", SessionID: ""},
		{},
	}
	for _, in := range cases {
		if _, err := e.Run(context.Background(), in); err == nil {
			t.Errorf("expected error for %+v", in)
		}
	}
}

// ---- Run() : app resolution errors ---------------------------------

func TestRun_AppLookupError(t *testing.T) {
	apps := &stubApps{err: errors.New("db down")}
	sess := &stubSessions{state: okState(t)}
	e := newEngine(t, apps, sess, okLLM())
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-x", SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "lookup app") {
		t.Fatalf("err = %v", err)
	}
	if apps.got != "app-x" {
		t.Errorf("AppID not propagated to lookup: %q", apps.got)
	}
}

func TestRun_NilAppOrDefinition(t *testing.T) {
	cases := []struct {
		name string
		app  *appmgr.RuntimeApp
	}{
		{"nil app", nil},
		{"nil definition", &appmgr.RuntimeApp{Meta: &appmgr.App{AppID: "app-1"}, Definition: nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			apps := &stubApps{app: c.app}
			e := newEngine(t, apps, &stubSessions{state: okState(t)}, okLLM())
			_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"})
			if err == nil || !strings.Contains(err.Error(), "Definition") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRun_NoAgents(t *testing.T) {
	apps := &stubApps{app: &appmgr.RuntimeApp{
		Meta:       &appmgr.App{AppID: "app-1"},
		Definition: &schema.AppDefinition{Agents: nil},
	}}
	e := newEngine(t, apps, &stubSessions{state: okState(t)}, okLLM())
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "no agents") {
		t.Fatalf("err = %v", err)
	}
}

// ---- Run() : session resolution errors -----------------------------

func TestRun_SessionStateError(t *testing.T) {
	apps := &stubApps{app: okApp(t, "you are helpful", "", schema.Brain{Provider: "anthropic", Model: "claude"})}
	sess := &stubSessions{stateErr: errors.New("not found")}
	e := newEngine(t, apps, sess, okLLM())
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-x"})
	if err == nil || !strings.Contains(err.Error(), "load session") {
		t.Fatalf("err = %v", err)
	}
	if sess.stateGot != "sess-x" {
		t.Errorf("SessionID not propagated: %q", sess.stateGot)
	}
}

func TestRun_NilState(t *testing.T) {
	apps := &stubApps{app: okApp(t, "you are helpful", "", schema.Brain{Provider: "anthropic", Model: "claude"})}
	sess := &stubSessions{state: nil}
	e := newEngine(t, apps, sess, okLLM())
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "no state") {
		t.Fatalf("err = %v", err)
	}
}

// ---- Run() : LLM errors --------------------------------------------

func TestRun_LLMError(t *testing.T) {
	apps := &stubApps{app: okApp(t, "p", "", schema.Brain{Provider: "anthropic", Model: "claude"})}
	sess := &stubSessions{state: okState(t)}
	lc := &stubLLM{err: errors.New("rate limit")}
	e := newEngine(t, apps, sess, lc)
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "llm chat") {
		t.Fatalf("err = %v", err)
	}
	// Turn lifecycle events ARE emitted (Started, PhaseChanged, Ended
	// with errored) — that's expected. The forbidden one is the
	// assistant message itself, which must not be persisted on LLM
	// failure.
	if got := sess.findAppend(sessionstore.EventAssistantMessage); got != nil {
		t.Errorf("EventAssistantMessage must not be appended on LLM error : %+v", got)
	}
	// And the turn must have ended with status=errored.
	end := sess.findAppend(sessionstore.EventTurnEnded)
	if end == nil {
		t.Fatal("EventTurnEnded must be emitted even on error")
	}
	if end.Turn == nil || end.Turn.Status != "errored" {
		t.Errorf("turn end status = %+v, want errored", end.Turn)
	}
}

func TestRun_LLMNilResponse(t *testing.T) {
	apps := &stubApps{app: okApp(t, "p", "", schema.Brain{Provider: "anthropic", Model: "claude"})}
	sess := &stubSessions{state: okState(t)}
	lc := &stubLLM{resp: nil}
	e := newEngine(t, apps, sess, lc)
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "nil response") {
		t.Fatalf("err = %v", err)
	}
	if got := sess.findAppend(sessionstore.EventAssistantMessage); got != nil {
		t.Errorf("EventAssistantMessage must not be appended when LLM nil : %+v", got)
	}
}

// ---- Run() : persistence error -------------------------------------

func TestRun_AppendDurableError(t *testing.T) {
	// Fail ONLY on the assistant message append (not on the lifecycle
	// events) — the original semantic was about the persist step
	// failing, not the lifecycle plumbing.
	apps := &stubApps{app: okApp(t, "p", "", schema.Brain{Provider: "anthropic", Model: "claude"})}
	sess := &stubSessions{
		state:           okState(t),
		appendErr:       errors.New("disk full"),
		appendErrOnType: sessionstore.EventAssistantMessage,
	}
	e := newEngine(t, apps, sess, okLLM())
	_, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "persist") {
		t.Fatalf("err = %v", err)
	}
}

// ---- Run() : happy path --------------------------------------------

func TestRun_HappyPath(t *testing.T) {
	brain := schema.Brain{Provider: "anthropic", Model: "claude-3-5-sonnet"}
	apps := &stubApps{app: okApp(t, "", "you are helpful", brain)}
	state := okState(t,
		sessionstore.Message{Seq: 1, Role: "user", Content: "hi"},
	)
	sess := &stubSessions{state: state, appendSeq: 42}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)

	res, err := e.Run(context.Background(), runtime.TurnInput{
		AppID:     "app-1",
		SessionID: "sess-1",
		UserJWT:   "tok-abc",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Seq != 42 {
		t.Errorf("Seq = %d, want 42", res.Seq)
	}
	if res.Content != "hello back" {
		t.Errorf("Content = %q, want %q", res.Content, "hello back")
	}

	// LLM request shape.
	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	if lc.got.BYOK {
		t.Error("V1 must be gateway mode (BYOK=false)")
	}
	if lc.got.Provider != "anthropic" || lc.got.Model != "claude-3-5-sonnet" {
		t.Errorf("brain not wired: provider=%q model=%q", lc.got.Provider, lc.got.Model)
	}
	if lc.got.UserJWT != "tok-abc" {
		t.Errorf("UserJWT = %q, want tok-abc", lc.got.UserJWT)
	}
	if len(lc.got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + user) ; got %+v", len(lc.got.Messages), lc.got.Messages)
	}
	if lc.got.Messages[0].Role != "system" || lc.got.Messages[0].Content != "you are helpful" {
		t.Errorf("system prompt not prepended: %+v", lc.got.Messages[0])
	}
	if lc.got.Messages[1].Role != "user" || lc.got.Messages[1].Content != "hi" {
		t.Errorf("user message not projected: %+v", lc.got.Messages[1])
	}

	// Assistant event persisted (one of many lifecycle events emitted).
	if !sess.appendCalled {
		t.Fatal("AppendDurable not called")
	}
	ev := sess.findAppend(sessionstore.EventAssistantMessage)
	if ev == nil {
		t.Fatal("EventAssistantMessage not appended")
	}
	if ev.SessionID != "sess-1" || ev.AppID != "app-1" {
		t.Errorf("event ids: session=%q app=%q", ev.SessionID, ev.AppID)
	}
	if ev.Message == nil || ev.Message.Role != "assistant" || ev.Message.Content != "hello back" {
		t.Errorf("event payload = %+v", ev.Message)
	}

	// Turn lifecycle events MUST be emitted on the happy path.
	if sess.findAppend(sessionstore.EventTurnStarted) == nil {
		t.Error("EventTurnStarted missing")
	}
	if got := sess.countAppend(sessionstore.EventTurnPhaseChanged); got != 3 {
		t.Errorf("EventTurnPhaseChanged count = %d, want 3 (loading|running|persisting)", got)
	}
	end := sess.findAppend(sessionstore.EventTurnEnded)
	if end == nil {
		t.Fatal("EventTurnEnded missing on happy path")
	}
	if end.Turn == nil || end.Turn.Status != "done" {
		t.Errorf("turn end status = %+v, want done", end.Turn)
	}
	// TurnID returned to caller matches the event's correlation.
	if res.TurnID == "" || res.TurnID != end.Turn.TurnID {
		t.Errorf("res.TurnID=%q, want match end.TurnID=%q", res.TurnID, end.Turn.TurnID)
	}
}

// ---- Run() : prompt fallback ---------------------------------------

func TestRun_PromptFallbackToAlias(t *testing.T) {
	// SystemPrompt empty → fall back to legacy `prompt` field.
	apps := &stubApps{app: okApp(t, "legacy prompt", "", schema.Brain{Provider: "x", Model: "y"})}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if len(lc.got.Messages) == 0 || lc.got.Messages[0].Role != "system" || lc.got.Messages[0].Content != "legacy prompt" {
		t.Errorf("legacy prompt not used: %+v", lc.got.Messages)
	}
}

func TestRun_NoSystemPromptWhenBothEmpty(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "x", Model: "y"})}
	sess := &stubSessions{state: okState(t, sessionstore.Message{Role: "user", Content: "hi"})}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range lc.got.Messages {
		if m.Role == "system" {
			t.Errorf("system message inserted despite empty prompts: %+v", m)
		}
	}
}

// ---- Run() : optional brain knobs ----------------------------------

func TestRun_TemperatureAndMaxTokensWired(t *testing.T) {
	temp := 0.42
	maxTok := 1234
	brain := schema.Brain{Provider: "x", Model: "y", Temperature: &temp, MaxTokens: &maxTok}
	apps := &stubApps{app: okApp(t, "p", "", brain)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.Temperature == nil || *lc.got.Temperature != 0.42 {
		t.Errorf("Temperature = %v, want 0.42", lc.got.Temperature)
	}
	if lc.got.MaxTokens == nil || *lc.got.MaxTokens != 1234 {
		t.Errorf("MaxTokens = %v, want 1234", lc.got.MaxTokens)
	}
}

func TestRun_TemperatureUnsetWhenNil(t *testing.T) {
	apps := &stubApps{app: okApp(t, "p", "", schema.Brain{Provider: "x", Model: "y"})}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.Temperature != nil {
		t.Errorf("Temperature must stay nil when brain doesn't set it; got %v", *lc.got.Temperature)
	}
	if lc.got.MaxTokens != nil {
		t.Errorf("MaxTokens must stay nil when brain doesn't set it; got %v", *lc.got.MaxTokens)
	}
}

// ---- Run() : routing depends on app.Meta.BYOK, not on brain ----------
//
// The compiler always requires a brain credential ; the runtime only
// honors it when the operator opted in via PUT /api/apps/{id}/byok.

func TestRun_AppBYOKFalse_RoutesGatewayEvenWithBrainCredential(t *testing.T) {
	// Brain has a real api_key (compile-time requirement). The app is
	// NOT in BYOK mode → engine MUST route through gateway, MUST
	// forward UserJWT, MUST NOT leak the brain api_key.
	brain := schema.Brain{
		Provider: "anthropic",
		Model:    "claude-3-5-sonnet",
		Config:   map[string]any{"api_key": "sk-ant-brain-key"},
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, false)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserJWT: "tok-user",
	}); err != nil {
		t.Fatal(err)
	}
	if lc.got.BYOK {
		t.Error("app.BYOK=false must keep BYOK request flag false (gateway mode)")
	}
	if lc.got.APIKey != "" {
		t.Errorf("brain api_key leaked in gateway mode : %q", lc.got.APIKey)
	}
	if lc.got.UserJWT != "tok-user" {
		t.Errorf("UserJWT not forwarded in gateway mode : %q", lc.got.UserJWT)
	}
}

func TestRun_AppBYOKTrue_UsesBrainAPIKeyAndBypassesGateway(t *testing.T) {
	brain := schema.Brain{
		Provider: "anthropic",
		Model:    "claude-3-5-sonnet",
		Config:   map[string]any{"api_key": "sk-ant-real-key"},
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, true)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserJWT: "ignored-in-byok",
	}); err != nil {
		t.Fatal(err)
	}
	if !lc.got.BYOK {
		t.Error("app.BYOK=true must flip request BYOK to true")
	}
	if lc.got.APIKey != "sk-ant-real-key" {
		t.Errorf("APIKey = %q, want sk-ant-real-key", lc.got.APIKey)
	}
}

func TestRun_AppBYOKTrue_BaseURLForwarded(t *testing.T) {
	// Self-hosted endpoint (Ollama, vLLM) lives in brain.config.base_url.
	// In BYOK mode the engine must forward it so Bifrost can dial it.
	brain := schema.Brain{
		Provider: "anthropic",
		Model:    "claude-3-5-sonnet",
		Config: map[string]any{
			"api_key":  "sk-ant-real-key",
			"base_url": "http://localhost:11434",
		},
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, true)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.BaseURL != "http://localhost:11434" {
		t.Errorf("BaseURL = %q", lc.got.BaseURL)
	}
}

func TestRun_AppBYOKTrue_CredentialStringPickedUp(t *testing.T) {
	brain := schema.Brain{
		Provider:   "anthropic",
		Model:      "claude-3-5-sonnet",
		Credential: "sk-ant-from-credential",
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, true)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.APIKey != "sk-ant-from-credential" {
		t.Errorf("APIKey = %q", lc.got.APIKey)
	}
}

func TestRun_AppBYOKTrue_CredentialMapPickedUp(t *testing.T) {
	brain := schema.Brain{
		Provider:   "anthropic",
		Model:      "claude-3-5-sonnet",
		Credential: map[string]any{"api_key": "sk-ant-from-map"},
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, true)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.APIKey != "sk-ant-from-map" {
		t.Errorf("APIKey = %q", lc.got.APIKey)
	}
}

func TestRun_AppBYOKTrue_ConfigAPIKeyWinsOverCredential(t *testing.T) {
	brain := schema.Brain{
		Provider:   "anthropic",
		Model:      "claude-3-5-sonnet",
		Config:     map[string]any{"api_key": "from-config"},
		Credential: "from-credential",
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, true)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.APIKey != "from-config" {
		t.Errorf("config.api_key should win over Credential ; got %q", lc.got.APIKey)
	}
}

func TestRun_AppBYOKFalse_BrainCredentialNeverLeaks(t *testing.T) {
	// Defense-in-depth : even with every brain credential surface
	// populated, BYOK=false on the app must keep the request strictly
	// gateway-mode with zero credential leakage.
	brain := schema.Brain{
		Provider:   "anthropic",
		Model:      "claude-3-5-sonnet",
		Config:     map[string]any{"api_key": "leak-1", "base_url": "http://leak"},
		Credential: map[string]any{"api_key": "leak-2"},
		ProviderID: "leak-3",
	}
	apps := &stubApps{app: okAppBYOK(t, "p", "", brain, false)}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserJWT: "tok-user",
	}); err != nil {
		t.Fatal(err)
	}
	if lc.got.BYOK {
		t.Fatal("BYOK leaked despite app.BYOK=false")
	}
	if lc.got.APIKey != "" || lc.got.BaseURL != "" {
		t.Errorf("credential leaked : APIKey=%q BaseURL=%q", lc.got.APIKey, lc.got.BaseURL)
	}
	if lc.got.UserJWT != "tok-user" {
		t.Errorf("UserJWT lost : %q", lc.got.UserJWT)
	}
}

// ---- Run() : ordering — first agent picked -------------------------

func TestRun_PicksFirstAgent(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "app-1"},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{
				{ID: "primary", SystemPrompt: "PRIMARY", Brain: schema.Brain{Provider: "p1", Model: "m1"}},
				{ID: "secondary", SystemPrompt: "SECONDARY", Brain: schema.Brain{Provider: "p2", Model: "m2"}},
			},
		},
	}
	apps := &stubApps{app: app}
	sess := &stubSessions{state: okState(t)}
	lc := okLLM()
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), runtime.TurnInput{AppID: "app-1", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if lc.got.Provider != "p1" || lc.got.Model != "m1" {
		t.Errorf("expected primary agent (p1/m1), got %s/%s", lc.got.Provider, lc.got.Model)
	}
	if lc.got.Messages[0].Content != "PRIMARY" {
		t.Errorf("expected PRIMARY system prompt, got %q", lc.got.Messages[0].Content)
	}
}
