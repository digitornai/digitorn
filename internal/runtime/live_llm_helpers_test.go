//go:build live

package runtime_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/core/servicebus"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	fsmod "github.com/digitornai/digitorn/internal/modules/filesystem"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/agent"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/worker"
)

// =====================================================================
// Live LLM test infrastructure
//
// All tests in this file are gated behind the `live` build tag AND the
// DIGITORN_LIVE_LLM=1 environment variable. They are NOT run as part
// of `go test ./...` ; they require :
//
//   go test -tags live ./internal/runtime/ -run TestLive
//
// plus environment :
//
//   DIGITORN_LIVE_LLM=1                — flag enabling the suite
//   DIGITORN_LLM_GATEWAY_URL           — daemon LLM gateway base URL
//   DIGITORN_LIVE_LLM_PROVIDER         — provider id ("openai")
//   DIGITORN_LIVE_LLM_MODEL            — model id ("gpt-4o-mini")
//   DIGITORN_LIVE_LLM_API_KEY          — BYOK key forwarded to the gateway
//
// Tests assert SEMANTIC correctness (the right tool was called, the
// result reached the next round, the gate veto landed) rather than
// exact strings — LLMs are non-deterministic.
// =====================================================================

// liveProvider returns provider/model/jwt/gatewayURL. JWT is the
// bearer token forwarded to the digitorn LLM gateway (gateway mode,
// the default). If no JWT is in env, the helper falls back to
// reading ~/.digitorn/credentials.json — the file the daemon's
// auth flow writes after a successful login.
func liveProvider(t *testing.T) (provider, model, jwt, gatewayURL string) {
	t.Helper()
	if os.Getenv("DIGITORN_LIVE_LLM") != "1" {
		t.Skip("DIGITORN_LIVE_LLM not set — skipping live LLM tests")
	}
	provider = os.Getenv("DIGITORN_LIVE_LLM_PROVIDER")
	if provider == "" {
		provider = "openai" // gateway speaks OpenAI-compat for everything
	}
	model = os.Getenv("DIGITORN_LIVE_LLM_MODEL")
	if model == "" {
		// Default to a small, fast, available gateway model.
		model = "copilot-gpt-4o-mini"
	}
	gatewayURL = os.Getenv("DIGITORN_LLM_GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:8002/v1"
	}
	jwt = os.Getenv("DIGITORN_DEV_JWT")
	if jwt == "" {
		jwt = readCredentialsJWT()
	}
	if jwt == "" {
		t.Skip("no DIGITORN_DEV_JWT in env AND no ~/.digitorn/credentials.json — skipping")
	}
	return
}

// readCredentialsJWT extracts access_token from
// ~/.digitorn/credentials.json. Returns "" on any failure
// (missing file, malformed JSON, missing field) so the caller
// can decide to skip.
func readCredentialsJWT() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".digitorn", "credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Tiny grep instead of pulling in encoding/json for a single field.
	const key = `"access_token"`
	idx := strings.Index(string(data), key)
	if idx == -1 {
		return ""
	}
	rest := string(data[idx+len(key):])
	startQuote := strings.Index(rest, `"`)
	if startQuote == -1 {
		return ""
	}
	rest = rest[startQuote+1:]
	endQuote := strings.Index(rest, `"`)
	if endQuote == -1 {
		return ""
	}
	return rest[:endQuote]
}

// =====================================================================
// Worker bootstrap : build once, spawn once via Manager.
// =====================================================================

var (
	liveWorkerOnce sync.Once
	liveWorkerExe  string
	liveWorkerErr  error
)

func buildLiveLLMWorker(t *testing.T) string {
	t.Helper()
	liveWorkerOnce.Do(func() {
		dir, err := os.MkdirTemp("", "digitorn-worker-llm-live-*")
		if err != nil {
			liveWorkerErr = err
			return
		}
		exe := filepath.Join(dir, "worker-llm")
		if goruntime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", exe,
			"github.com/digitornai/digitorn/cmd/digitorn-worker-llm")
		cmd.Stdout = io.Discard
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			liveWorkerErr = err
			return
		}
		liveWorkerExe = exe
	})
	if liveWorkerErr != nil {
		t.Fatalf("build worker-llm: %v", liveWorkerErr)
	}
	return liveWorkerExe
}

func liveLLMClient(t *testing.T) *llm.Client {
	t.Helper()
	_, _, _, gatewayURL := liveProvider(t)
	exe := buildLiveLLMWorker(t)

	mgr := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mgr.Start(); err != nil {
		t.Fatalf("worker.Manager.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Stop(ctx)
	})

	env := map[string]string{}
	if gatewayURL != "" {
		env["DIGITORN_LLM_GATEWAY_URL"] = gatewayURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := mgr.Spawn(ctx, worker.Spec{
		Kind:         "llm",
		Binary:       exe,
		Count:        1,
		Env:          env,
		StartTimeout: 20 * time.Second,
	}); err != nil {
		t.Fatalf("worker spawn: %v", err)
	}

	client, err := llm.NewClient(llm.ClientConfig{
		Manager: mgr,
		Kind:    "llm",
		Retries: 1,
		Timeout: 60 * time.Second,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("llm.NewClient: %v", err)
	}
	return client
}

// =====================================================================
// Engine fixture
// =====================================================================

// liveSessions is the live fixture's session store : per-session projected
// state (so a sub-agent's sub-session NEVER shares the coordinator's state —
// mirroring the production SessionStore's hard isolation) PLUS a flat global
// event log the assertion helpers scan. The single-state projectingSessions
// stub used elsewhere can't host multi-agent runs : the sub-agent's messages
// would bleed into the parent's history and break tool-call/result pairing.
type liveSessions struct {
	mu     sync.Mutex
	seq    uint64
	events []sessionstore.Event
	states map[string]*sessionstore.SessionState
}

func newLiveSessions() *liveSessions {
	return &liveSessions{states: map[string]*sessionstore.SessionState{}}
}

func (p *liveSessions) stateLocked(sid string) *sessionstore.SessionState {
	st := p.states[sid]
	if st == nil {
		st = sessionstore.NewSessionState(sid)
		p.states[sid] = st
	}
	return st
}

func (p *liveSessions) State(sid string) (*sessionstore.SessionState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stateLocked(sid), nil
}

func (p *liveSessions) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	ev.Seq = p.seq
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	p.events = append(p.events, ev)
	sessionstore.Apply(p.stateLocked(ev.SessionID), &ev)
	return p.seq, nil
}

func (p *liveSessions) Append(ctx context.Context, ev sessionstore.Event) (uint64, error) {
	return p.AppendDurable(ctx, ev)
}

// Lock-safe accessors : the abort tests read the event log WHILE the engine
// goroutine is still appending, so direct f.session.events iteration would race.

func (p *liveSessions) countType(t sessionstore.EventType) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.events {
		if p.events[i].Type == t {
			n++
		}
	}
	return n
}

func (p *liveSessions) lastAssistantTextOf(sid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var last string
	for i := range p.events {
		ev := p.events[i]
		if ev.SessionID != sid || ev.Type != sessionstore.EventAssistantMessage || ev.Message == nil {
			continue
		}
		for _, part := range ev.Message.Parts {
			if part.Type == sessionstore.PartTypeText {
				last = part.Text
			}
		}
	}
	return last
}

func (p *liveSessions) hasTurnStatus(status string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.events {
		ev := p.events[i]
		if ev.Type == sessionstore.EventTurnEnded && ev.Turn != nil && ev.Turn.Status == status {
			return true
		}
	}
	return false
}

type liveEngineFixture struct {
	engine     *dgruntime.Engine
	session    *liveSessions
	workspace  string
	llmClient  *llm.Client
	app        *appmgr.RuntimeApp
	apps       *stubApps
	caps       *schema.CapabilitiesConfig
	userJWT    string // forwarded as TurnInput.UserJWT for gateway routing
	busAdapter *dispatch.BusAdapter
	agents     *agent.Manager
}

func liveSetup(t *testing.T) *liveEngineFixture {
	t.Helper()
	provider, model, jwt, _ := liveProvider(t)

	workspace := t.TempDir()

	// Bus + filesystem rooted in the workspace.
	bus := servicebus.New()
	fs := fsmod.New()
	if err := fs.Init(context.Background(), map[string]any{
		"workspace": workspace,
	}); err != nil {
		t.Fatalf("filesystem init: %v", err)
	}
	if err := bus.Register(fs); err != nil {
		t.Fatalf("bus register: %v", err)
	}

	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{
				Name:        "filesystem.read",
				Description: "Read the contents of a file from the workspace.",
				RiskLevel:   tool.RiskLow,
				Params: []tool.ParamSpec{
					{Name: "path", Type: "string", Description: "Path relative to the workspace.", Required: true},
				},
			}},
		{Module: "filesystem", Action: "write",
			Spec: &tool.Spec{
				Name:        "filesystem.write",
				Description: "Write content to a file in the workspace.",
				RiskLevel:   tool.RiskMedium,
				Params: []tool.ParamSpec{
					{Name: "path", Type: "string", Required: true},
					{Name: "content", Type: "string", Required: true},
				},
			}},
		{Module: "filesystem", Action: "ls",
			Spec: &tool.Spec{
				Name:        "filesystem.ls",
				Description: "List directory contents.",
				RiskLevel:   tool.RiskLow,
				Params: []tool.ParamSpec{
					{Name: "path", Type: "string", Required: true},
				},
			}},
		{Module: "filesystem", Action: "grep",
			Spec: &tool.Spec{
				Name:        "filesystem.grep",
				Description: "Search file contents matching a regex.",
				RiskLevel:   tool.RiskLow,
				Params: []tool.ParamSpec{
					{Name: "pattern", Type: "string", Required: true},
					{Name: "path", Type: "string"},
				},
			}},
	}

	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}

	// Gateway mode (BYOK=false) : the JWT we read from ~/.digitorn
	// is forwarded as UserJWT via the TurnInput. Bifrost dials the
	// digitorn gateway with that Bearer.
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{
			AppID:   "live-app",
			Enabled: true,
			BYOK:    false,
		},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{
				AppID: "live-app", Name: "Live Test", Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:   "main",
				Role: "assistant",
				Brain: schema.Brain{
					Provider: provider,
					Model:    model,
				},
				SystemPrompt: "You are a helpful assistant with filesystem tools. When the user asks you to read, write, list or search files, you MUST call the appropriate tool. Don't guess file contents — always read them. Be concise.",
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: caps,
				// Declare the opt-in modules per the documented YAML contract so
				// their tools are offered to the live agent : memory.* (memory
				// module) + agent_spawn.agent (loaded → the delegation tool, used
				// by the multi-agent live test once it promotes agent[0] to
				// coordinator).
				Modules: map[string]schema.ModuleBlock{
					"memory":      {},
					"agent_spawn": {},
				},
			},
			Runtime: &schema.RuntimeBlock{
				ToolInjection: schema.ToolInjectionDirect,
			},
		},
		BundleDir: workspace,
	}

	apps := &stubApps{app: app}
	sess := newLiveSessions()

	lcClient := liveLLMClient(t)

	cb := wiring.New(staticActionsSource{all: universe})
	ba := dispatch.NewBusAdapter(bus)
	disp := &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex {
			return cb.IndexFor(appID, agentID)
		},
		Inner: ba,
	}

	e, err := dgruntime.New(apps, sess, lcClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	e.Context = cb
	disp.Gate = e   // match production : gate meta sub-tools (execute_tool, …)
	disp.Memory = e // MEM : working-memory tools (set_goal / remember / task_*)

	// MA : wire the multi-agent orchestrator so the `agent` delegation tool
	// works live. The engine is the sub-agent runner ; coordinator gating reads
	// the role from the app definition.
	am := agent.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	am.AttachRunner(e)
	am.AttachSink(sess) // durable agent_spawn/agent_result → resync via state.Children
	disp.Agents = liveAgentAdapter{m: am}
	disp.CoordinatorLookup = func(appID, agentID string) bool {
		ra, _ := apps.Get(context.Background(), appID)
		if ra == nil || ra.Definition == nil {
			return false
		}
		for i := range ra.Definition.Agents {
			if ra.Definition.Agents[i].ID == agentID {
				return ra.Definition.Agents[i].Role == "coordinator"
			}
		}
		return false
	}
	e.Dispatcher = disp

	return &liveEngineFixture{
		engine:     e,
		session:    sess,
		workspace:  workspace,
		llmClient:  lcClient,
		app:        app,
		apps:       apps,
		caps:       caps,
		userJWT:    jwt,
		busAdapter: ba,
		agents:     am,
	}
}

// injectUser appends a user message event onto the projection so
// the LLM adapter includes it in the next prompt.
func (f *liveEngineFixture) injectUser(t *testing.T, text string) {
	t.Helper()
	ev := sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: "live-sess",
		AppID:     "live-app",
		UserID:    "test-user",
		Message: &sessionstore.MessagePayload{
			Role: "user",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: text},
			},
		},
	}
	if _, err := f.session.AppendDurable(context.Background(), ev); err != nil {
		t.Fatalf("inject user message: %v", err)
	}
}

// runLive : inject the user prompt then run one turn with a long-ish
// timeout (LLM calls can take 30s+ in worst-case).
func (f *liveEngineFixture) runLive(t *testing.T, userMessage string) {
	t.Helper()
	f.injectUser(t, userMessage)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := f.engine.Run(ctx, dgruntime.TurnInput{
		AppID:     "live-app",
		SessionID: "live-sess",
		UserID:    "test-user",
		UserJWT:   f.userJWT,
	}); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
}

// =====================================================================
// Semantic assertion helpers
// =====================================================================

// assertSemantic checks that the LLM's final assistant message
// contains AT LEAST ONE of the case-insensitive substrings. Used
// when the LLM might phrase things differently across runs.
func assertSemantic(t *testing.T, f *liveEngineFixture, expectedAny ...string) {
	t.Helper()
	got := finalAssistantText(f)
	low := strings.ToLower(got)
	for _, want := range expectedAny {
		if strings.Contains(low, strings.ToLower(want)) {
			return
		}
	}
	t.Errorf("no expected substring %v in assistant message : %q", expectedAny, got)
}

// assertSemanticNotIn fails if any forbidden substring appears in
// the final assistant message.
func assertSemanticNotIn(t *testing.T, f *liveEngineFixture, forbidden ...string) {
	t.Helper()
	got := finalAssistantText(f)
	low := strings.ToLower(got)
	for _, bad := range forbidden {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Errorf("forbidden substring %q found in assistant message : %q", bad, got)
		}
	}
}

func finalAssistantText(f *liveEngineFixture) string {
	var last string
	for _, ev := range f.session.events {
		if ev.SessionID != "live-sess" {
			continue // ignore sub-agent sub-sessions — only the top-level reply counts
		}
		if ev.Type == sessionstore.EventAssistantMessage && ev.Message != nil {
			for _, p := range ev.Message.Parts {
				if p.Type == sessionstore.PartTypeText {
					last = p.Text
				}
			}
		}
	}
	return last
}

func assertToolCalled(t *testing.T, f *liveEngineFixture, toolName string) {
	t.Helper()
	if !toolWasCalled(f, toolName) {
		t.Errorf("tool %q was not called", toolName)
		for _, ev := range f.session.events {
			if ev.Type == sessionstore.EventToolCall && ev.Tool != nil {
				t.Logf("  saw tool_call : %s", ev.Tool.Name)
			}
		}
	}
}

func assertToolNotCalled(t *testing.T, f *liveEngineFixture, toolName string) {
	t.Helper()
	if toolWasCalled(f, toolName) {
		t.Errorf("tool %q must NOT have been called", toolName)
	}
}

// canonicalToolName accepts either the dotted FQN ("filesystem.read") or
// the OpenAI wire-form ("filesystem__read") and returns BOTH so callers
// can match against persisted events regardless of which form ended up
// in storage. The runtime canonicalises inbound tool_calls back to dots
// (meta.Canonicalize) but older fixtures may have stored either form.
func canonicalToolNames(name string) (string, string) {
	if idx := strings.Index(name, "."); idx != -1 {
		return name, name[:idx] + "__" + name[idx+1:]
	}
	if idx := strings.Index(name, "__"); idx != -1 {
		return name[:idx] + "." + name[idx+2:], name
	}
	return name, name
}

func toolWasCalled(f *liveEngineFixture, toolName string) bool {
	dotted, underscored := canonicalToolNames(toolName)
	for _, ev := range f.session.events {
		if ev.Type == sessionstore.EventToolCall && ev.Tool != nil {
			if ev.Tool.Name == dotted || ev.Tool.Name == underscored {
				return true
			}
		}
	}
	return false
}

func countToolCalls(f *liveEngineFixture, toolName string) int {
	dotted, underscored := canonicalToolNames(toolName)
	n := 0
	for _, ev := range f.session.events {
		if ev.Type == sessionstore.EventToolCall && ev.Tool != nil {
			if ev.Tool.Name == dotted || ev.Tool.Name == underscored {
				n++
			}
		}
	}
	return n
}

func (f *liveEngineFixture) writeWorkspaceFile(t *testing.T, path, content string) {
	t.Helper()
	full := filepath.Join(f.workspace, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
