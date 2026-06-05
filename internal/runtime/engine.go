// Package runtime orchestrates one agent turn. The Engine here is a
// thin façade : its Run method owns input validation, recovery of
// stale turns, and delegation to the turn.Turn state machine which in
// turn (no pun intended) emits every lifecycle event durably and
// holds the bounded pool slots that keep the daemon stable at 1M+
// concurrent sessions.
//
// Engine deliberately stays compact ; runtime growth (tool dispatch,
// hooks, approvals, multi-agent, streaming) lands as sibling packages
// under internal/runtime/ and is invoked from Engine via well-typed
// extension hooks added in their respective sprints.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	coremw "github.com/mbathepaul/digitorn/internal/core/middleware"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/ports"
	"github.com/mbathepaul/digitorn/internal/runtime/adapter"
	"github.com/mbathepaul/digitorn/internal/runtime/behavior"
	"github.com/mbathepaul/digitorn/internal/runtime/context/prompt"
	"github.com/mbathepaul/digitorn/internal/runtime/contextcompact"
	"github.com/mbathepaul/digitorn/internal/runtime/contextsvc"
	"github.com/mbathepaul/digitorn/internal/runtime/errclass"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/policy/approval"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
	"github.com/mbathepaul/digitorn/internal/runtime/turn"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
	"github.com/mbathepaul/digitorn/internal/safego"
)

// PathPolicySource resolves the per-session workdir PathPolicy (WD). The daemon
// implements it over the session store + merged module constraints. ok=false
// means the session has no workdir → path-typed args are not policy-confined
// at the chokepoint and the module's own static resolution applies.
type PathPolicySource interface {
	PathPolicyFor(appID, sessionID string) (workdir.PathPolicy, bool)
}

// AppLookup is the slice of the App Manager the engine needs. The
// real implementation is *appmgr.gormManager (via appmgr.Manager) ;
// tests can substitute a stub returning a RuntimeApp directly.
type AppLookup interface {
	Get(ctx context.Context, appID string) (*appmgr.RuntimeApp, error)
}

// SessionAccess is the slice of the session store the engine needs.
// *sessionstore.Bus satisfies this in production ; mock for tests.
type SessionAccess interface {
	State(sid string) (*sessionstore.SessionState, error)
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

// LLMChat is the slice of the LLM client the engine needs.
// *llm.Client satisfies this in production ; fake for tests.
type LLMChat interface {
	Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

// LLMStream is the optional streaming-capable LLM contract. When
// the wired LLMChat ALSO satisfies LLMStream and Engine.Streaming
// is true, the engine uses ChatStream and emits per-token
// EventAssistantDelta events on the session bus, so subscribers
// (Socket.IO, CLI) can render tokens as they arrive.
//
// Falling back is automatic : if either condition is false, the
// engine uses the synchronous Chat path with no behavioural
// change for callers that don't care about streaming.
type LLMStream interface {
	ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan *llm.ChatChunk, error)
}

// BlobLoader is the slice of the blob store the engine needs to inline
// binary message parts (images, audio, files) before handing the
// conversation to the LLM. nil = no blob support, binary parts will be
// skipped with a logged warning. The daemon wires *blobstore.Store ;
// tests pass an in-memory stub or nil.
type BlobLoader interface {
	LoadBlob(ctx context.Context, hash string) ([]byte, error)
}

// Runner is the public surface the HTTP layer depends on. *Engine
// satisfies it ; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, in TurnInput) (*TurnResult, error)
}

// Engine drives one agent turn. Wired once at boot from the daemon.
// Pool gates concurrent turn execution (3 tiers : global / app /
// user) ; nil Pool means unbounded (acceptable for unit tests but
// NEVER for the live daemon).
// EmergencyCompactor performs an aggressive mid-turn compaction when the LLM
// rejects a prompt for exceeding its context window. Satisfied by the same
// SessionCompactor the hook engine drives ; a local interface keeps the engine
// decoupled from the server package.
type EmergencyCompactor interface {
	CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error
}

type Engine struct {
	Apps       AppLookup
	Sessions   SessionAccess
	LLM        LLMChat
	Blobs      BlobLoader     // optional ; nil = binary message parts skipped
	Tools      ToolCatalog    // optional ; nil falls back to NoToolsCatalog
	Dispatcher ToolDispatcher // optional ; nil falls back to NoopDispatcher
	// Compactor recovers from a mid-turn context overflow : auto_compact should
	// prevent it, but one huge tool result can blow past the window in a single
	// step. nil = no emergency recovery (the overflow propagates as the turn's
	// failure).
	Compactor EmergencyCompactor
	// ContextTouch signals the background Context Service that a session's
	// context changed and should be recounted EXACTLY (off the turn loop).
	// nil = no background recompute (the provider anchor still keeps the gauge
	// exact per turn). MUST be non-blocking — it is called on the hot path.
	ContextTouch func(sessionID string)
	// ContextRecordParts reports the assembled system prompt + tool schemas at
	// request-build so the background Context Service can split the EXACT
	// occupancy into system / tools / messages (CTX-7). nil = no breakdown.
	// MUST be non-blocking (hot build path).
	ContextRecordParts func(sessionID string, system, tools []string)
	// ContextLookup returns the freshest context variable for a session — the
	// in-memory ContextView the runtime keeps current from the Context Service
	// notifications (leads the durable projection). The per-round compaction
	// guard and the hook metrics read it so pressure tracks the REAL context
	// even mid-turn. nil → fall back to the projection snapshot.
	ContextLookup func(sessionID string) (contextsvc.ContextView, bool)
	// ContextRecordRatio persists the self-calibrated provider-tokens /
	// local-estimate ratio learned from a round's exact prompt_tokens, so the
	// next turn's budget guard targets the model's REAL count (Claude/Gemini have
	// no local tokenizer). nil = not recorded (still calibrates within a turn).
	ContextRecordRatio func(sessionID string, ratio float64)
	Pool               *turn.Pool

	// taskSeq is a per-session monotonic counter for task ids (t1, t2, …).
	// task_create calls in a turn dispatch in PARALLEL and the durable todo
	// projection is write-behind, so deriving the next id by reading the
	// projection races and hands out colliding ids (every task got "t1").
	// An atomic counter, seeded once from durable state, keeps every id
	// unique even under a rapid batch. Keyed by session id -> *int64.
	taskSeq sync.Map

	// SubAgentPool gates SUB-AGENT turns. It is deliberately separate from
	// Pool (the user-turn pool) and unbounded by default : a sub-agent must
	// never contend for a user-turn slot, otherwise a coordinator holding a
	// slot while it waits for a child that needs one deadlocks the moment the
	// pool fills. Sub-agent concurrency is bounded elsewhere (the per-call LLM
	// semaphore), so the turn-pool tier for sub-agents stays a no-op.
	SubAgentPool *turn.Pool

	// LLMSem bounds CONCURRENT LLM calls daemon-wide — the real bottleneck
	// (provider rate-limits / gateway capacity). It is a counting semaphore
	// acquired immediately before each LLM call and released immediately
	// after, so it is NEVER held while an agent waits on a tool or a
	// sub-agent. That single property is what lets a million nested agents
	// run without any agent blocking another : a waiting agent holds nothing
	// scarce, so its children can always acquire a slot. nil = unbounded
	// (tests / dev).
	LLMSem chan struct{}

	IDGen  turn.IDGen
	Logger *slog.Logger

	// PolicyEvaluator runs the documented security gates on every
	// tool_call before it reaches the dispatcher (SG-4). Opt-in : nil
	// = no enforcement, matching the doc's "dev/test mode" (the
	// capabilities block being absent in YAML has the same effect on
	// the in-process flow). The daemon wires a concrete evaluator
	// from the app's capabilities + agent definition.
	PolicyEvaluator PolicyEvaluator

	// PathPolicies resolves the per-session workdir PathPolicy (WD). The
	// engine attaches it to the dispatch context once per turn so the single
	// chokepoint (enforceGate) confines every path-typed tool arg to the
	// session's workdir — top-level AND meta paths. nil = no workdir
	// enforcement (dev/test, or apps with no workdir).
	PathPolicies PathPolicySource

	// ApprovalRegistry is the process-local synchronisation point
	// between the engine (which suspends a tool_call when gate 4
	// resolves to "approve") and the HTTP/Socket.IO surface (which
	// signals the resolution back). Opt-in : nil = approvals fall
	// back to the SG-4 placeholder ("errored / approval needed").
	// Daemon wires *approval.Registry at boot.
	ApprovalRegistry *approval.Registry

	// Context is the context_builder integration (CB-6). When set,
	// the engine consults it per turn to obtain the per-agent tool
	// list + the assembled system prompt (the 9 documented sections,
	// with user's system_prompt last). When nil, the engine falls
	// back to the basic Tools/SystemPrompt path used by RT-3 tests.
	//
	// Production daemons wire this via the context_builder wiring
	// helper (internal/runtime/context/wiring).
	Context ContextBuilder

	// Hooks is an optional source of hook engines per app. When set,
	// the engine fires turn_start / turn_end / pre_tool_use /
	// post_tool_use at the documented moments (RT-4). Nil = no hook
	// pipeline ; the engine behaves exactly as before.
	//
	// The lookup is per-app so a hot redeploy can swap an app's
	// hook set without affecting other apps' in-flight turns.
	Hooks HookSource

	// MaxToolIterations caps the LLM ↔ tool ping-pong inside a single
	// turn. Without a cap, a misbehaving model that keeps calling tools
	// could loop forever ; this is the hard safety belt. Default 10 ;
	// override at construction.
	MaxToolIterations int

	// ToolTimeout bounds a SINGLE tool dispatch. A tool that exceeds it is
	// cancelled and returns an errored outcome the model sees and reacts to —
	// the turn keeps going. This is what stops one slow/hung tool (a broad
	// grep, a wedged shell) from eating the whole turn : it must be shorter
	// than the runner's idle watchdog so a long tool can't be mistaken for a
	// stalled turn. Modules that need longer set their own (e.g. bash) or run
	// via background_run. Zero = no engine-level per-tool bound.
	ToolTimeout time.Duration

	// BackgroundNotifications is an optional source of per-session
	// completion notifications from background_run. When wired,
	// the engine drains pending entries at turn_start and injects
	// each as a synthetic user-role message in the doc format :
	//
	//   [BACKGROUND TASK COMPLETED] task_id=... tool=... elapsed=...s
	//
	// per docs-site/language/04c-primitives.md "Auto-notification".
	// Nil = no notifications (the LLM polls bg tasks manually).
	BackgroundNotifications BackgroundNotifier

	// Streaming (R-4) enables per-token EventAssistantDelta emission
	// when the wired LLMChat also satisfies LLMStream. When false
	// or the LLM client doesn't support streaming, the engine uses
	// the synchronous Chat path — no behavioural change. Existing
	// callers default to non-streaming, so this is purely additive.
	Streaming bool

	// ResponseNormalizer is an optional post-processor applied to every LLM
	// response before tool dispatch. It exists so models that emit tool calls
	// as TEXT (no native tool_calls) can be normalised by an external library
	// (internal/llm.NormalizeTextToolCalls) without the turn loop knowing any
	// format. Nil = disabled ; a no-op for well-behaved providers.
	ResponseNormalizer func(*llm.ChatResponse)

	// behaviorEngines caches one behavior.Engine per app (keyed by app
	// id), built lazily from security.behavior. The engine holds the
	// per-session counters/sets/flags, so it must persist across turns ;
	// the cache gives every turn of an app the same instance. Guarded by
	// behaviorMu. Inert for apps without a security.behavior block.
	behaviorMu      sync.Mutex
	behaviorEngines map[string]*behavior.Engine

	// MiddlewareRetriever is the optional RAG retrieval seam for the
	// rag_inject middleware. nil = rag_inject is inert (no knowledge-base
	// backend), matching the reference daemon. Wired by the daemon when a KB
	// backend exists.
	MiddlewareRetriever coremw.Retriever

	// MiddlewareCustomFactory resolves a `custom` middleware entry into an
	// AppMiddleware (the gRPC plugin transport). nil = custom middleware is
	// unavailable. Wired by the daemon over the worker manager.
	MiddlewareCustomFactory func(name string, cfg map[string]any) (ports.AppMiddleware, error)

	// middlewarePipes caches one app-level middleware Pipeline per app
	// (keyed by app id), built lazily from runtime.middleware. nil entry =
	// the app declares no middleware (the pipeline is skipped at zero cost).
	middlewareMu    sync.Mutex
	middlewarePipes map[string]*coremw.Pipeline
}

// middlewareFor returns the per-app app-level middleware pipeline (lazily
// built, cached), or nil when the app declares no runtime.middleware. The
// cached value may itself be nil (app has middleware key but it resolves to
// nothing) — both mean "no pipeline".
func (e *Engine) middlewareFor(app *appmgr.RuntimeApp) *coremw.Pipeline {
	if app == nil || app.Definition == nil || app.Definition.Runtime == nil ||
		len(app.Definition.Runtime.Middleware) == 0 {
		return nil
	}
	appID := ""
	if app.Meta != nil {
		appID = app.Meta.AppID
	}
	e.middlewareMu.Lock()
	defer e.middlewareMu.Unlock()
	if e.middlewarePipes == nil {
		e.middlewarePipes = map[string]*coremw.Pipeline{}
	}
	if p, ok := e.middlewarePipes[appID]; ok {
		return p
	}
	p := coremw.Build(app.Definition.Runtime.Middleware, coremw.Deps{
		Retriever:     e.MiddlewareRetriever,
		CustomFactory: e.MiddlewareCustomFactory,
	}, e.Logger)
	e.middlewarePipes[appID] = p
	return p
}

// behaviorFor returns the per-app behavior engine (lazily built, cached),
// or nil when the app declares no security.behavior block (enforcement is
// opt-in). The per-session state lives inside the returned engine, so all
// turns of one app share it.
func (e *Engine) behaviorFor(app *appmgr.RuntimeApp) *behavior.Engine {
	if app == nil || app.Definition == nil || app.Definition.Security == nil ||
		app.Definition.Security.Behavior == nil {
		return nil
	}
	appID := ""
	if app.Meta != nil {
		appID = app.Meta.AppID
	}
	e.behaviorMu.Lock()
	defer e.behaviorMu.Unlock()
	if e.behaviorEngines == nil {
		e.behaviorEngines = map[string]*behavior.Engine{}
	}
	if be, ok := e.behaviorEngines[appID]; ok {
		return be
	}
	be := behavior.New(app.Definition.Security.Behavior)
	e.behaviorEngines[appID] = be
	return be
}

// CleanupBehaviorSession drops a session's in-memory behavior state for the
// given app, bounding the per-session map on session deletion. No-op when the
// app has no behavior engine. The server calls this from deleteSession via an
// optional interface (mirrors the reference daemon's cleanup_session).
func (e *Engine) CleanupBehaviorSession(appID, sid string) {
	e.behaviorMu.Lock()
	be := e.behaviorEngines[appID]
	e.behaviorMu.Unlock()
	if be != nil {
		be.CleanupSession(sid)
	}
}

// defaultMaxToolIterations is the per-turn ceiling on LLM↔tool rounds when an
// agent declares none. Deliberately HIGH: a real agentic task (scaffold + npm
// install + run a server + read several files) legitimately needs many rounds,
// and the old low cap silently cut turns mid-task. It stays bounded only as a
// runaway-loop backstop; apps tune it with agents[].max_tool_iterations. When
// the ceiling IS reached, the turn ends with a VISIBLE note, never silently.
const defaultMaxToolIterations = 100

// defaultMaxStopVetoes caps how many times the `stop` hook may hold a single
// turn open (veto the model's attempt to finish + inject a steering directive)
// before the runtime lets the turn end regardless. It is the anti-loop
// backstop : a genuinely stuck guard (e.g. a task that can't be completed)
// can never wedge the loop or burn unbounded tokens. The agent is reminded,
// then trusted to finish. Apps override via runtime.max_stop_retries (0
// disables stop-hook holds entirely).
const defaultMaxStopVetoes = 2

// resolveMaxStopVetoes reads the app's runtime.max_stop_retries override,
// falling back to defaultMaxStopVetoes when unset. A configured value
// (including 0) is honoured verbatim.
func resolveMaxStopVetoes(rt *schema.RuntimeBlock) int {
	if rt != nil && rt.MaxStopRetries != nil {
		if v := *rt.MaxStopRetries; v >= 0 {
			return v
		}
	}
	return defaultMaxStopVetoes
}

// loadBlob is the adapter glue between the runtime Engine and the
// adapter package's BlobLoader signature. Returns (nil, nil) when no
// blob store is wired so the adapter cleanly skips binary parts
// without spurious errors during smoke tests.
func (e *Engine) loadBlob(ctx context.Context, hash string) ([]byte, error) {
	if e.Blobs == nil {
		return nil, nil
	}
	return e.Blobs.LoadBlob(ctx, hash)
}

// tools returns the wired catalog, falling back to the no-op catalog
// when none was configured. Keeps the runPhases body uncluttered with
// nil checks while preserving the "agent has no tools" V0 path.
func (e *Engine) tools() ToolCatalog {
	if e.Tools == nil {
		return NoToolsCatalog{}
	}
	return e.Tools
}

// buildAssistantParts converts a ChatResponse into the multipart shape
// we persist. The text payload (if any) becomes a single text Part ;
// every tool_call becomes a Part of type ToolCall. Order matters —
// providers return text-then-tool_call, we keep that ordering so the
// projection / UI can render the assistant's reasoning before its
// tool invocations.
func buildAssistantParts(resp *llm.ChatResponse) []sessionstore.MessagePart {
	if resp == nil {
		return nil
	}
	var parts []sessionstore.MessagePart
	if resp.Content != "" {
		parts = append(parts, sessionstore.MessagePart{
			Type: sessionstore.PartTypeText,
			Text: resp.Content,
		})
	}
	for _, tc := range resp.ToolCalls {
		parts = append(parts, sessionstore.MessagePart{
			Type: sessionstore.PartTypeToolCall,
			ToolCall: &sessionstore.ToolCallSpec{
				ID:   tc.ID,
				Name: tc.Name,
				Args: tc.Arguments,
			},
		})
	}
	return parts
}

// slogReporter adapts *slog.Logger to the adapter.Reporter interface.
// Lets the adapter emit structured warnings (skipped parts, missing
// blobs) into the daemon's normal log stream.
type slogReporter struct {
	log *slog.Logger
}

func (r *slogReporter) Warn(msg string, kv ...any) {
	if r == nil || r.log == nil {
		return
	}
	r.log.Warn(msg, kv...)
}

// compile-time guard.
var _ Runner = (*Engine)(nil)

// New constructs an Engine. Apps, Sessions, llmClient are required.
// Pool defaults to an unbounded pool (cfg{} = all caps zero) — fine
// for tests but the daemon MUST supply a real Pool sized for its
// hardware. IDGen defaults to uuid.NewString.
func New(apps AppLookup, sessions SessionAccess, llmClient LLMChat, logger *slog.Logger) (*Engine, error) {
	if apps == nil {
		return nil, errors.New("runtime: nil AppLookup")
	}
	if sessions == nil {
		return nil, errors.New("runtime: nil SessionAccess")
	}
	if llmClient == nil {
		return nil, errors.New("runtime: nil LLMChat")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		Apps:         apps,
		Sessions:     sessions,
		LLM:          llmClient,
		Pool:         turn.NewPool(turn.PoolConfig{}),
		SubAgentPool: turn.NewPool(turn.PoolConfig{}), // unbounded : deadlock-free nesting
		IDGen:        uuid.NewString,
		Logger:       logger,
	}, nil
}

// TurnInput is everything the engine needs to drive one turn. AppID
// and SessionID are required. UserJWT rides through to the LLM in
// gateway mode (default). UserID is used by the pool's per-user tier
// to prevent one user from starving others.
type TurnInput struct {
	AppID     string
	SessionID string
	UserJWT   string
	UserID    string
	// Mode is the composer mode id the caller selected (runtime.modes).
	// Empty falls back to the session's sticky active mode, then the app
	// default-policy (auto → first declared). Resolved per turn.
	Mode string

	// AgentID selects which declared agent runs this turn (its logical
	// YAML id). Empty = the entry agent (runtime.entry_agent, else the
	// first declared agent). The AgentManager sets this when running an
	// isolated sub-agent turn.
	AgentID string

	// AgentRunID is the distinct per-instance identity attributed to the
	// gateway/provider and the telemetry registry — e.g. "coding#a1b2c3"
	// so two concurrent instances of the same specialist are
	// distinguishable. Empty = use the agent's logical id (the entry
	// agent's stable identity).
	AgentRunID string

	// SubAgent marks this turn as a delegated sub-agent run. It uses the
	// engine's unbounded SubAgentPool instead of the user-turn Pool, so
	// nested delegation can never deadlock on a full user-turn pool.
	SubAgent bool
}

// TurnResult is what the engine returns. Seq is the session-store
// sequence number of the assistant message that was appended (the
// caller can use it to subscribe to subsequent events). Content
// echoes the LLM's reply for observability ; the bridge already
// pushed the same content to Socket.IO via the AppendDurable
// notification.
type TurnResult struct {
	Seq     uint64
	Content string
	TurnID  string
}

// Run executes a single turn end-to-end. The flow is :
//
//  1. Validate inputs (fast-fail, no Turn allocated).
//  2. Load the app + session snapshot (still pre-Turn so we can
//     recover stale turns BEFORE acquiring a new pool slot).
//  3. RecoverInFlight : if the snapshot shows a turn was in-flight at
//     the last daemon restart, emit EventError + EventTurnEnded for
//     it so the projection clears state.CurrentTurn*.
//  4. Allocate Turn + Start (acquires pool, emits EventTurnStarted).
//  5. Phase Loading : build LLM messages from the (now refreshed)
//     session snapshot + prepended system prompt.
//  6. Phase Running : dispatch the LLM ChatRequest with routing
//     decided by app.Meta.BYOK (gateway vs direct).
//  7. Phase Persisting : AppendDurable the assistant reply.
//  8. CloseDone (emits EventTurnEnded{status: done}, releases pool).
//
// Any error path between Start and CloseDone calls Turn.Fail to emit
// EventTurnEnded{status: errored} + release the pool, so resources
// are never leaked.
// errModeTimeout is the context cause attached to the per-turn mode timeout, so
// context.Cause(ctx) tells a mode-timeout cancellation apart from the daemon
// safety cutoff and a client abort (see the turn-interrupted Warn in Run).
var errModeTimeout = errors.New("turn mode timeout exceeded")

func (e *Engine) Run(ctx context.Context, in TurnInput) (*TurnResult, error) {
	if in.AppID == "" || in.SessionID == "" {
		return nil, errors.New("runtime: AppID and SessionID required")
	}

	// Carry the gateway bearer on the ctx for the WHOLE turn so EVERY mid-turn
	// LLM call that doesn't receive the TurnInput authenticates to the gateway
	// like the main turn does — notably context compaction's summary brain. That
	// call can be triggered by the per-round guard (already on the turn ctx) OR
	// by an auto_compact hook firing at turn_start / turn_end, which fireHook
	// runs on THIS ctx. Wrapping only inside runPhases left the hook path with no
	// token, so a hook-driven summarize silently degraded to truncate. Wrapping
	// here covers session_start / turn_start / turn_end / runPhases alike.
	ctx = llm.WithUserJWT(ctx, in.UserJWT)

	// 1. App + agent.
	app, err := e.Apps.Get(ctx, in.AppID)
	if err != nil {
		return nil, fmt.Errorf("runtime: lookup app %q: %w", in.AppID, err)
	}
	if app == nil || app.Definition == nil {
		return nil, fmt.Errorf("runtime: app %q has no Definition", in.AppID)
	}
	if len(app.Definition.Agents) == 0 {
		return nil, fmt.Errorf("runtime: app %q has no agents", in.AppID)
	}
	agent := resolveAgent(app.Definition, in.AgentID)
	if agent == nil {
		return nil, fmt.Errorf("runtime: app %q has no agent %q", in.AppID, in.AgentID)
	}

	// 2. Session snapshot.
	state, err := e.Sessions.State(in.SessionID)
	if err != nil {
		return nil, fmt.Errorf("runtime: load session %q: %w", in.SessionID, err)
	}
	if state == nil {
		return nil, fmt.Errorf("runtime: session %q has no state", in.SessionID)
	}
	preSnap := state.Snapshot()

	// 3. Recover any in-flight turn left over by a previous daemon
	// crash. Idempotent : 0 stale turns = no-op.
	if _, err := turn.RecoverInFlight(ctx, preSnap, e.Sessions); err != nil {
		return nil, fmt.Errorf("runtime: recover in-flight: %w", err)
	}

	// 4. Allocate + start the Turn. From this point on, every error
	// path MUST call tr.Fail to release pool slots + emit
	// EventTurnEnded.
	pool := e.Pool
	if in.SubAgent && e.SubAgentPool != nil {
		pool = e.SubAgentPool
	}
	tr, err := turn.New(turn.Options{
		SessionID: in.SessionID,
		AppID:     in.AppID,
		AgentID:   agent.ID,
		UserID:    in.UserID,
		UserJWT:   in.UserJWT,
		Pool:      pool,
		Sink:      e.Sessions,
		IDGen:     e.IDGen,
		Logger:    e.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: new turn: %w", err)
	}
	if err := tr.Start(ctx); err != nil {
		return nil, fmt.Errorf("runtime: start turn: %w", err)
	}

	// session_start : fires ONCE, on the first turn of the session
	// (doc : "First turn of a session (turn == 0)"). TurnCount counts
	// user messages ; on the first turn the just-posted user message
	// makes it 1, so <=1 is the first-turn signal. Fires BEFORE
	// turn_start so a setup hook (e.g. session_start + module_action to
	// bootstrap state, per the doc) runs before anything else.
	if preSnap.TurnCount <= 1 {
		ssRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventSessionStart, hooks.Payload{
			AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
		})
		e.applyInjections(ctx, in, tr, ssRes.Injects)
	}

	// RT-4 : fire the canonical turn_start hook (alias user_prompt
	// resolves to the same event per 31-tool-hooks.md). Doc-conform
	// : hooks run between the turn-start barrier and the first LLM
	// call ; failures never block the turn.
	turnStartPayload := withTurnState(hooks.Payload{
		AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
	}, e.computeHookMetrics(state.Snapshot(), agent, "", 0))
	turnStartRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventTurnStart, turnStartPayload)
	// inject_message on turn_start lands BEFORE the snapshot below, so
	// the injected message is part of the first LLM call this turn.
	e.applyInjections(ctx, in, tr, turnStartRes.Injects)

	// 04c-primitives.md "Auto-notification" : drain any completed
	// background_run tasks and inject them as synthetic user
	// messages so the LLM sees them in the upcoming round.
	e.injectBackgroundNotifications(ctx, in, tr.ID)

	// Re-snapshot after recovery + notification injection so the
	// new messages land in `snap.Messages` for the first LLM call.
	snap := state.Snapshot()

	res, endMetrics, runErr := e.runPhases(ctx, tr, app, agent, snap, in)
	if runErr != nil {
		// RT-6 : distinguish user-initiated cancellation from real
		// failures. ctx.Canceled / ctx.DeadlineExceeded → Interrupt
		// (status="interrupted" in EventTurnEnded). Everything else
		// → Fail (status="errored"). The split lets the UI render
		// "stopped" vs "crashed" properly and keeps audit accurate.
		// We use a fresh background context for the close emit so a
		// cancelled parent ctx doesn't prevent persisting the
		// terminal event itself.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if isCancellation(ctx, runErr) {
			reason := runErr.Error()
			// Prefer the named cause over the anonymous "context canceled" so the
			// interrupted turn says WHY (client abort vs. idle safety cutoff).
			if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
				reason = cause.Error()
			} else if cerr := ctx.Err(); cerr != nil {
				reason = cerr.Error()
			}
			// Idle safety cutoff (not a user abort) : leave a VISIBLE note so the
			// turn doesn't just vanish after a tool. The runner coalesces a
			// follow-up, so "send another message to continue" actually resumes.
			if errors.Is(context.Cause(ctx), ErrTurnSafetyCutoff) {
				e.persistInterruptedAssistant(tr, in,
					"[Turn stopped: no progress for the safety window — the task may be stuck (a tool that never returned, or a hung model call). Send another message to continue.]")
				// A safety cutoff is a STALL, not a user abort : surface it as a
				// real error event so the masked provider/tool hang reaches the
				// client instead of a silent "interrupted".
				e.emitTurnError(closeCtx, in, tr, context.Cause(ctx))
			}
			// Surface WHY the turn was cut short — a cancellation mid-flight
			// (notably the LLM call that resumes a turn after an ask_user /
			// approval pause) otherwise vanishes with no turn_complete and no
			// turn_failed log. context.Cause names the trigger : a client abort,
			// the per-turn mode timeout, or the wall-clock safety cutoff. The
			// last two mean a human-in-the-loop pause likely consumed the turn's
			// execution deadline — the cause that single-run stack capture can't
			// see (a timer closes Done() without calling a cancel func).
			e.Logger.Warn("runtime: turn interrupted before completion",
				slog.String("turn_id", tr.ID),
				slog.String("session_id", in.SessionID),
				slog.String("app_id", in.AppID),
				slog.String("reason", reason),
				slog.Any("cause", context.Cause(ctx)))
			if closeErr := tr.Interrupt(closeCtx, reason); closeErr != nil {
				e.Logger.Warn("runtime: tr.Interrupt emit error",
					slog.String("turn_id", tr.ID),
					slog.String("err", closeErr.Error()))
			}
			return nil, runErr
		}
		if closeErr := tr.Fail(closeCtx, runErr); closeErr != nil {
			e.Logger.Warn("runtime: tr.Fail emit error",
				slog.String("turn_id", tr.ID),
				slog.String("err", closeErr.Error()))
		}
		// Client-facing error event (DaemonError shape) so web/flutter render
		// the failure and offer Retry — turn_ended{errored} alone never reaches
		// their error banner.
		e.emitTurnError(closeCtx, in, tr, runErr)
		// error : an exception escaped the agent loop (doc semantics).
		// ErrorType carries the failure string so the `error_type`
		// condition (regex) can match. Uses closeCtx so a cancelled
		// parent can't suppress the hook ; inject_message effects (e.g.
		// a recovery note) are persisted for the next turn.
		errRes := e.fireHook(closeCtx, in.AppID, agent, schema.HookEventError, hooks.Payload{
			AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID,
			TurnID: tr.ID, ErrorType: runErr.Error(),
		})
		e.applyInjections(closeCtx, in, tr, errRes.Injects)
		return nil, runErr
	}
	if err := tr.CloseDone(ctx); err != nil {
		// Done event failed to persist : the assistant message is
		// already durable, so surface a soft warning but return success.
		e.Logger.Warn("runtime: tr.CloseDone emit error",
			slog.String("turn_id", tr.ID),
			slog.String("err", err.Error()))
	}
	// RT-4 : fire turn_end on the happy path. The hook engine gets a
	// fresh background-derived ctx so a cancelled parent doesn't
	// short-circuit observability emissions.
	turnEndRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventTurnEnd, withTurnState(hooks.Payload{
		AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
	}, endMetrics))
	// inject_message on turn_end queues a message for the NEXT turn.
	e.applyInjections(ctx, in, tr, turnEndRes.Injects)
	res.TurnID = tr.ID
	return res, nil
}

// runPhases drives the turn through the agent loop : LLM call → tool
// dispatch → LLM call → ... until the model returns no more tool_calls
// (the final answer) or we hit MaxToolIterations.
//
// Phase transitions are coarse : Loading once at start, Running for the
// whole loop, Persisting at the end. The lifecycle observers (TUI
// timeline, web realtime) get fine-grained EventToolCall +
// EventToolResult events per iteration, which is what they actually
// need — phase changes per round would be noise.
//
// ISOLATION CONTRACT : every tool call is dispatched in its own
// goroutine via ToolDispatcher. The turn's goroutine waits via
// WaitGroup. Other sessions' turns are not affected — they have their
// own Engine.Run goroutines and their own dispatcher invocations.
func (e *Engine) runPhases(
	ctx context.Context,
	tr *turn.Turn,
	app *appmgr.RuntimeApp,
	agent *schema.Agent,
	snap sessionstore.SessionSnapshot,
	in TurnInput,
) (*TurnResult, hooks.Payload, error) {
	// Carry the gateway bearer on the ctx so mid-turn LLM calls that don't get
	// the TurnInput — context compaction's summary brain — authenticate to the
	// gateway like the main turn does (which sets ChatRequest.UserJWT directly).
	// Without this the summary call hits the gateway with no token and silently
	// falls back to truncate.
	ctx = llm.WithUserJWT(ctx, in.UserJWT)
	if err := tr.TransitionTo(ctx, turn.PhaseLoading); err != nil {
		return nil, hooks.Payload{}, fmt.Errorf("runtime: phase loading: %w", err)
	}

	// CB-6 : when a ContextBuilder is wired, it owns tool list AND
	// system prompt assembly. Otherwise we fall back to the basic
	// agent.SystemPrompt + e.Tools.ToolsForAgent path used by RT-3
	// tests.
	var (
		systemPrompt string
		tools        []llm.ToolSpec
	)
	if e.Context != nil {
		appName := ""
		appVersion := ""
		if app != nil {
			if app.Definition != nil {
				appName = app.Definition.App.Name
				appVersion = app.Definition.App.Version
			}
			if appName == "" && app.Meta != nil {
				appName = app.Meta.AppID
			}
		}
		// memory + agent_spawn are opt-in modules per the documented YAML
		// contract : memory is gated by declaring tools.modules.memory ;
		// agent_spawn by loading the module (declared or granted). Only then
		// do we offer their tools. The memory snapshot is re-rendered from the
		// live state every turn so it survives context compaction AND resume.
		memEnabled := appMemoryEnabled(app)
		agentEnabled := appAgentSpawnEnabled(app)
		var memView *prompt.WorkingMemoryView
		if memEnabled {
			memView = workingMemoryView(snap)
		}
		// Non-universal context_builder primitives are offered only when
		// actually usable : the bridge must be wired, AND (for ask_user /
		// use_skill) the app must opt in. Otherwise the model is shown a tool
		// that returns "not wired" or has nothing to act on — pure noise that
		// derails small models.
		callAppEnabled, askUserWired, useSkillWired := false, false, false
		if pa, ok := e.Dispatcher.(primitiveAvailability); ok {
			callAppEnabled = pa.CallAppWired()
			askUserWired = pa.AskUserWired()
			useSkillWired = pa.UseSkillWired()
		}
		ctxRes, err := e.Context.BuildFor(ctx, ContextRequest{
			App:             app,
			Agent:           agent,
			AppName:         appName,
			AppVersion:      appVersion,
			MemoryEnabled:   memEnabled,
			AgentEnabled:    agentEnabled,
			CallAppEnabled:  callAppEnabled,
			AskUserEnabled:  askUserWired && appGrantsAskUser(app),
			UseSkillEnabled: useSkillWired && appHasSkills(app, agent),
			Memory:          memView,
		})
		if err != nil {
			return nil, hooks.Payload{}, fmt.Errorf("runtime: context_builder: %w", err)
		}
		systemPrompt = ctxRes.SystemPrompt
		tools = ctxRes.Tools
	} else {
		systemPrompt = agent.SystemPrompt
		if systemPrompt == "" {
			systemPrompt = agent.Prompt
		}
		tools = e.tools().ToolsForAgent(agent)
	}

	// Behavioral enforcement (security.behavior) : the per-app engine holds
	// the per-session counters/sets/flags. Reset per-turn state at the top of
	// the turn. nil when the app declares no behavior block.
	be := e.behaviorFor(app)
	if be != nil {
		be.OnTurnStart(in.SessionID)
	}

	// App-level middleware (runtime.middleware) : per-app pipeline, resolved
	// once per turn. nil = no middleware (skipped at zero cost).
	mwPipe := e.middlewareFor(app)

	// Composer mode (runtime.modes) : resolve the effective mode (sticky),
	// filter the offered tools to its allow-list, announce a switch as a
	// durable system directive when it changed (re-snapshots so it lands in
	// this turn), and derive the per-turn caps + dispatch guard. Inert when
	// the app declares no modes.
	modeGate, modeMaxTurns, modeTimeout, behaviorProfile := e.applyTurnMode(ctx, tr, app, in, &snap, &tools)
	if modeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, time.Duration(modeTimeout*float64(time.Second)), errModeTimeout)
		defer cancel()
	}

	// behavior_profile swap (doc point 6) : the mode re-resolves the active
	// rules against its profile while per-session counters/sets/flags survive.
	// Empty profile reverts to security.behavior.profile ; same profile is a
	// no-op. The enforced-rules section is then appended to the system prompt
	// so the model knows the rules it will be held to this turn.
	if be != nil {
		be.SetActiveProfile(in.SessionID, behaviorProfile)
		if bp := be.PromptText(in.SessionID); bp != "" {
			if systemPrompt != "" {
				systemPrompt += "\n\n" + bp
			} else {
				systemPrompt = bp
			}
		}
	}

	// WD : tell the agent its working directory. Injected per-turn here (NOT in
	// the cached BuildFor prompt, which is keyed per app+agent and would leak
	// one session's workdir to another). Uses the SAME policy the chokepoint
	// enforces, so the prompt states exactly the confined root.
	if e.PathPolicies != nil {
		if pp, ok := e.PathPolicies.PathPolicyFor(in.AppID, in.SessionID); ok && pp.HasWorkdir() {
			block := "# Working directory\n" +
				"You are working in: " + pp.Root() + "\n" +
				"All file paths are relative to this directory. You can only read and write files inside it — access to anything outside is denied."
			if systemPrompt != "" {
				systemPrompt += "\n\n" + block
			} else {
				systemPrompt = block
			}
		}
	}

	// BYOK routing decided once per turn (doesn't change across rounds).
	var (
		apiKey  string
		baseURL string
	)
	if app.Meta != nil && app.Meta.BYOK {
		apiKey, baseURL = extractEmbeddedAuth(agent.Brain)
	}

	if err := tr.TransitionTo(ctx, turn.PhaseRunning); err != nil {
		return nil, hooks.Payload{}, fmt.Errorf("runtime: phase running: %w", err)
	}

	maxIter := e.MaxToolIterations
	if maxIter <= 0 {
		maxIter = defaultMaxToolIterations
	}
	// The agent's YAML overrides the default (raise it for long build tasks, or
	// lower it to keep a turn short) — agents[].max_tool_iterations.
	if agent.MaxToolIterations != nil && *agent.MaxToolIterations > 0 {
		maxIter = *agent.MaxToolIterations
	}
	// A mode may narrow the per-turn loop bound (e.g. Ask mode caps at 8).
	if modeMaxTurns > 0 && modeMaxTurns < maxIter {
		maxIter = modeMaxTurns
	}
	// Anti-loop cap for stop-hook holds this turn (runtime.max_stop_retries).
	maxStopHold := resolveMaxStopVetoes(app.Definition.Runtime)

	// Behavioral classifier (classify_turns) : a small pre-turn LLM pass that
	// analyses the upcoming turn and injects a director directive. Best-effort,
	// bounded by its own timeout, never fatal. Runs before round 1 so the
	// directive is in context. Inert unless the app enables classify_turns.
	if be != nil && be.ClassifyEnabled() {
		e.runBehaviorClassifier(ctx, tr, app, agent, be, in, &snap, tools, apiKey, baseURL)
	}

	var (
		lastSeq       uint64
		lastContent   string
		lastModel     string
		usage         llm.Usage
		toolCallsUsed int  // cumulative across rounds — feeds tool_calls condition
		finalAnswer   bool // true once the model returns text with no tool calls
		stopVetoes    int  // stop-hook holds applied this turn (anti-loop cap)
	)
	emergencyCompacted := false // at most one mid-turn context-overflow recovery per turn

	// One Converter per turn : it caches the per-message LLM conversion by
	// Seq across iterations, so the growing history is only converted at its
	// tail and blobs are loaded once. Output is identical to a fresh
	// MessagesToLLM(viewMsgs) on every call (proven in adapter tests).
	conv := adapter.NewConverter(adapter.Options{
		LoadBlob: e.loadBlob,
		Report:   &slogReporter{log: e.Logger},
	})
	// Per-round compaction guard policy : the same auto_compact knobs the
	// compiler reads (runtime.context, brain.context fallback). guardKeep is the
	// adaptive keep_recent — it shrinks as the guard fires so a still-over
	// context converges instead of re-keeping the same huge tool-result tail.
	compactPol := resolveAutoCompact(app.Definition.Runtime, agent.Brain.Context)
	guardKeep := compactPol.keep
	// Self-calibrated provider-tokens / local-estimate ratio, seeded from the
	// session's last calibration (if any) and refined after each round below.
	calibRatio := defaultEstimateRatio
	if cv := e.freshContextView(in.SessionID, snap, agent.Brain); cv.EstimateRatio > 0 {
		calibRatio = cv.EstimateRatio
	}
	for iter := 0; iter < maxIter; iter++ {
		// RT-6 : honour user-initiated cancellation between
		// iterations. ctx.Done() can fire BETWEEN an LLM round and
		// the next one ; checking here prevents one extra LLM call
		// (and one extra token bill) when the user already pressed
		// stop. The LLM.Chat call below carries ctx as well, so
		// in-flight requests cancel ; this check just short-circuits
		// the loop preamble.
		if err := ctx.Err(); err != nil {
			return nil, hooks.Payload{}, fmt.Errorf("runtime: turn cancelled at iter %d: %w", iter, err)
		}
		// Tell the runner's safety watchdog the turn is still advancing, so a
		// long-but-productive turn (slow tools, many rounds) is never killed —
		// only a genuine stall (no progress for the whole idle window) trips it.
		PingTurnKeepalive(ctx)

		// Per-ROUND context guarantee : before building/sending this round,
		// read the FRESH context variable and compact NOW if pressure crossed
		// the threshold. This catches a turn whose prior tool results ballooned
		// the context — the auto_compact hook (turn_start) can't, since it only
		// fires once per turn. snap is reloaded in place to the compacted view,
		// so the build below shrinks automatically.
		e.guardContextPressure(ctx, in, agent, &snap, compactPol, &guardKeep, usage.PromptTokens)

		// Build the LLM request from the current session snapshot. On
		// the FIRST iteration `snap` is the entry snapshot ; on
		// subsequent iterations we reload after appending tool results.
		// Context compaction : when a compaction marker is present, the
		// model sees the COMPACTED VIEW (summary system message + recent
		// messages) instead of the full history. The on-disk history is
		// untouched ; only the prompt shrinks. Reproducible after resume
		// from the durable marker.
		msgs := e.buildLLMMessages(ctx, conv, snap, systemPrompt, in.SessionID, agent.Brain)

		req := &llm.ChatRequest{
			BYOK:      app.Meta != nil && app.Meta.BYOK,
			Provider:  agent.Brain.Provider,
			Model:     agent.Brain.Model,
			APIKey:    apiKey,
			BaseURL:   baseURL,
			UserJWT:   in.UserJWT,
			Messages:  msgs,
			Tools:     tools,
			SessionID: in.SessionID,
			UserID:    in.UserID,
			AgentID:   agentRunID(in.AgentRunID, agent.ID),
		}
		// CTX-7 breakdown : report the assembled system prompt + tool schemas
		// (the parts not in the session) so the background recount can split
		// system / tools / messages. Non-blocking ; the messages bucket comes
		// from the live projection.
		e.recordContextParts(in.SessionID, systemPrompt, tools)
		if agent.Brain.Temperature != nil {
			req.Temperature = agent.Brain.Temperature
		}
		if agent.Brain.MaxTokens != nil {
			req.MaxTokens = agent.Brain.MaxTokens
		}

		// App middleware Before : may mutate the system prompt + user messages
		// (mask_secrets, prompt_inject, rag_inject) or short-circuit the LLM
		// (content_filter). Runs per LLM round, like the reference daemon.
		var (
			mctx           *ports.MiddlewareContext
			shortCircuited bool
			resp           *llm.ChatResponse
			sentEst        int // local-estimate of the prompt actually sent (for ratio calibration)
		)
		if mwPipe != nil {
			mctx = buildMiddlewareContext(agent, in, iter, systemPrompt, req.Messages)
			scResp, sc, mwErr := mwPipe.Before(ctx, mctx)
			if mwErr != nil {
				return nil, hooks.Payload{}, fmt.Errorf("runtime: middleware before (iter %d): %w", iter, mwErr)
			}
			applyMiddlewareContext(req.Messages, mctx, systemPrompt)
			if sc {
				shortCircuited = true
				resp = &llm.ChatResponse{Content: scResp, Model: "middleware"}
			}
		}

		if !shortCircuited {
			// HARD budget guarantee : measure the prompt actually built for this
			// round and compact until it fits the window — BEFORE sending. Closes
			// the case the async per-round guard misses (several big tool results
			// in one round outrun the background recount).
			e.enforcePromptBudget(ctx, in, agent, conv, &snap, req, systemPrompt, compactPol, &guardKeep, calibRatio)
			sentEst = estReqTokens(req)
			r, err := e.chatOrStream(ctx, tr, in, req)
			if err != nil && e.Compactor != nil && !emergencyCompacted && contextcompact.IsContextOverflow(err) {
				// Context overflow : the prompt blew past the model's window in a
				// single step (usually one huge tool result). Aggressively
				// truncate the session, rebuild the now-smaller prompt and retry
				// ONCE per turn. The LLM is refusing, so truncate costs no call.
				emergencyCompacted = true
				keep := 0
				if agent.Brain.Context != nil {
					keep = agent.Brain.Context.KeepRecent
				}
				if cerr := e.Compactor.CompactSession(ctx, in.SessionID, contextcompact.StrategyTruncate, contextcompact.EmergencyKeepRecent(keep)); cerr == nil {
					if st, serr := e.Sessions.State(in.SessionID); serr == nil && st != nil {
						snap = st.Snapshot()
						vm := snap.Messages
						if cc := snap.ContextCompaction; cc != nil && cc.CutoffSeq > 0 {
							vm = contextcompact.ApplyView(snap.Messages, cc.CutoffSeq, cc.Summary)
						}
						req.Messages = conv.Convert(ctx, vm)
						if systemPrompt != "" {
							req.Messages = append([]llm.ChatMessage{{Role: "system", Content: systemPrompt}}, req.Messages...)
						}
						snipOversizedMessages(req.Messages, e.msgByteCap(in.SessionID, snap, agent.Brain))
						r, err = e.chatOrStream(ctx, tr, in, req)
					}
				}
			}
			if err != nil {
				// Interrupted mid-generation : the streamed deltas are NOT
				// projected into the durable message list, so persist the
				// partial answer as an assistant message — with a detached ctx
				// so the cancellation itself can't suppress the save. The user
				// keeps what was generated before "stop".
				if r != nil && strings.TrimSpace(r.Content) != "" {
					e.persistInterruptedAssistant(tr, in, r.Content)
				}
				return nil, hooks.Payload{}, fmt.Errorf("runtime: llm chat (iter %d): %w", iter, err)
			}
			if r == nil {
				return nil, hooks.Payload{}, fmt.Errorf("runtime: llm returned nil response (iter %d)", iter)
			}
			resp = r
		}

		// Recover tool calls from models that emit them as text (no native
		// tool_calls). Delegated entirely to the wired normalizer — the loop
		// holds no format knowledge. No-op for native-tool providers.
		if e.ResponseNormalizer != nil {
			e.ResponseNormalizer(resp)
		}

		// App middleware After : transforms the response (response_filter,
		// mask_secrets). Runs even after a short-circuit, with empty tool calls
		// — mirrors the reference daemon.
		if mwPipe != nil {
			out, mwErr := mwPipe.After(ctx, mctx, resp.Content, toPortsToolCalls(resp.ToolCalls))
			if mwErr != nil {
				return nil, hooks.Payload{}, fmt.Errorf("runtime: middleware after (iter %d): %w", iter, mwErr)
			}
			resp.Content = out
		}

		// CANONICALISATION RACINE : the LLM wire uses underscored tool
		// names (planner sanitization) ; everything internal — hook
		// payloads, policy evaluation, persistence, projection — speaks
		// the dotted FQN. Convert once at the boundary so the entire
		// downstream pipeline sees the canonical form. Idempotent on
		// names already in dot form.
		// Ref : docs-site/language/04-tools.md "Tool name sanitization".
		canonicalizeToolCallNames(resp.ToolCalls, tools)

		// Persist the assistant message — text + tool_call parts.
		seq, err := e.persistAssistantStep(ctx, tr, in, resp)
		if err != nil {
			return nil, hooks.Payload{}, err
		}
		lastSeq = seq
		lastContent = resp.Content
		lastModel = resp.Model
		usage = resp.Usage

		// Self-calibrate the estimate ratio from this round's EXACT prompt_tokens
		// vs our local estimate of the very prompt we just sent. This teaches the
		// budget guard the model's real tokenisation density (Claude/Gemini have
		// no local tokenizer) so the next round/turn targets the true limit.
		if sentEst > 0 && usage.PromptTokens > 0 {
			r := float64(usage.PromptTokens) / float64(sentEst)
			if r < 1.0 {
				r = 1.0
			} else if r > 4.0 {
				r = 4.0
			}
			calibRatio = r
			if e.ContextRecordRatio != nil {
				e.ContextRecordRatio(in.SessionID, r)
			}
		}

		// Behavioral enforcement runs SEQUENTIALLY and IN ORDER around the
		// parallel tool dispatch : the engine bookkeeping (consecutive-tool,
		// plan-stated, counters) is inherently ordered, so doing it inside
		// the parallel goroutines would both race and scramble the counters.
		// on_text first (marks plan stated + fires on_text rules), then the
		// per-call pre-pass, then the post-pass below.
		var beNotes []behavior.Violation
		if be != nil {
			beNotes = append(beNotes, be.OnAgentText(in.SessionID, resp.Content)...)
		}

		if len(resp.ToolCalls) == 0 {
			// No more tools : the model wants to END the turn. Flush any
			// on_text directive first.
			e.injectBehaviorNotes(ctx, in, tr, beNotes)
			// `stop` hook : a directive hook (e.g. the built-in task-completion
			// guard) may VETO the finish and steer the agent instead of letting
			// it stop. The veto is honoured at most maxStopHold times per turn
			// so a stuck guard can never wedge the loop.
			if stopVetoes < maxStopHold {
				stopRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventStop,
					withTurnState(hooks.Payload{
						AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
					}, e.computeHookMetrics(snap, agent, resp.Content, toolCallsUsed)))
				if stopRes.Gate != nil && !stopRes.Gate.Allow {
					stopVetoes++
					// Carry the steering text into THIS turn : inject_message
					// effects first, else the gate Reason itself. Persisted as a
					// durable system directive ; the re-snapshot below makes the
					// next round see it.
					if len(stopRes.Injects) > 0 {
						e.applyInjections(ctx, in, tr, stopRes.Injects)
					} else if r := strings.TrimSpace(stopRes.Gate.Reason); r != "" {
						// Ride the authoritative directive protocol so the model
						// treats it as binding, not a suggestion (the observed
						// non-compliance failure mode).
						e.injectSystemDirective(ctx, in, tr.ID,
							wrapRuntimeDirective("stop", "critical", r),
							DirectiveHookInject, map[string]any{"hook": "stop"}, nil)
					}
					if st, sErr := e.Sessions.State(in.SessionID); sErr == nil && st != nil {
						snap = st.Snapshot()
					}
					e.Logger.Info("runtime: stop hook held the turn open",
						slog.String("session_id", in.SessionID),
						slog.Int("veto", stopVetoes))
					continue
				}
			} else {
				e.Logger.Warn("runtime: stop-veto cap reached; ending turn",
					slog.String("session_id", in.SessionID),
					slog.Int("cap", maxStopHold))
			}
			finalAnswer = true
			break
		}

		// Persist EventToolCall (status="pending") per call. These feed
		// the timeline AND signal "tool dispatch starting" to consumers.
		e.persistToolCallEvents(ctx, tr, in, resp.ToolCalls)

		// Turn-state metrics for the tool_start / tool_end hook payloads :
		// count THIS round's calls into the cumulative tool_calls total,
		// and carry the assistant's content for content_contains.
		toolCallsUsed += len(resp.ToolCalls)
		metrics := e.computeHookMetrics(snap, agent, resp.Content, toolCallsUsed)

		// Behavior pre-tool pass : a block-level violation prevents the call
		// from executing (its message becomes the synthetic error result) ;
		// warn/remind violations are accumulated and injected after results.
		var beBlocks map[int]string
		if be != nil {
			beBlocks = map[int]string{}
			for i, tc := range resp.ToolCalls {
				for _, v := range be.PreTool(in.SessionID, tc.Name, tc.Arguments, resp.Content) {
					if v.Level == "block" {
						beBlocks[i] = v.Format()
					} else {
						beNotes = append(beNotes, v)
					}
				}
			}
		}

		// Dispatch all tool calls IN PARALLEL. Each runs in its own
		// goroutine inside the dispatcher's execution lane.
		// The policy evaluator (SG-4) runs per-call BEFORE Dispatch ;
		// see dispatchToolsParallel.
		outcomes := e.dispatchToolsParallel(ctx, tr, in, app, agent, resp.ToolCalls, metrics, modeGate, beBlocks)

		// Tools finished : reset the idle watchdog before the (possibly slow)
		// result persistence + next LLM round, so a batch that took most of the
		// window doesn't trip the cutoff on the very next step.
		PingTurnKeepalive(ctx)

		// Persist EventToolResult per outcome. The projection appends a
		// "tool" role Message so the next iteration's LLM call sees the
		// results in context.
		if err := e.persistToolResults(ctx, tr, in, resp.ToolCalls, outcomes); err != nil {
			return nil, hooks.Payload{}, err
		}

		// Behavior post-tool pass : update tracking state + collect reminders
		// for executed calls (blocked calls never ran, so they are skipped).
		if be != nil {
			for i, tc := range resp.ToolCalls {
				if _, blocked := beBlocks[i]; blocked {
					continue
				}
				beNotes = append(beNotes, be.PostTool(in.SessionID, tc.Name, tc.Arguments, outcomeToResult(outcomes[i]))...)
			}
		}
		// One durable behavior directive carries this round's warn/remind
		// notes ; it lands after the tool results so the model sees the
		// guidance alongside them on the next round.
		e.injectBehaviorNotes(ctx, in, tr, beNotes)

		// Re-snapshot the session so the next iteration sees the tool
		// results in MessagesToLLM. Tolerant on State() failure : keep
		// the last snap and let the loop terminate or retry.
		if st, sErr := e.Sessions.State(in.SessionID); sErr == nil && st != nil {
			snap = st.Snapshot()
		}
	}

	// The loop ran out of tool-iteration budget without the model producing a
	// final answer. End VISIBLY — persist an assistant note and carry it as the
	// result — instead of cutting the turn silently (which read as "it just
	// stopped"). The agent/user can send one more message to continue.
	if !finalAnswer {
		note := fmt.Sprintf("[Turn stopped after %d tool steps without finishing — the task may be incomplete. Send another message to continue. (An app can raise agents[].max_tool_iterations.)]", maxIter)
		if strings.TrimSpace(lastContent) != "" {
			note = lastContent + "\n\n" + note
		}
		if seq, perr := e.persistAssistantStep(ctx, tr, in, &llm.ChatResponse{Content: note}); perr == nil {
			lastSeq = seq
		}
		lastContent = note
		e.Logger.Warn("runtime: turn hit tool-iteration limit",
			slog.String("session_id", in.SessionID),
			slog.String("app_id", in.AppID),
			slog.Int("max_iter", maxIter))
	}

	if err := tr.TransitionTo(ctx, turn.PhasePersisting); err != nil {
		return nil, hooks.Payload{}, fmt.Errorf("runtime: phase persisting: %w", err)
	}

	e.Logger.Info("runtime: turn complete",
		slog.String("turn_id", tr.ID),
		slog.String("app_id", in.AppID),
		slog.String("session_id", in.SessionID),
		slog.String("model", lastModel),
		slog.Int("tokens_in", usage.PromptTokens),
		slog.Int("tokens_out", usage.CompletionTokens),
		slog.Uint64("seq", lastSeq),
	)
	// CTX-7 ANCHOR : persist the provider's authoritative token usage. This is
	// the exact size of the context the model just processed — the numerator of
	// context_pressure. Until now usage was only logged, so the gauge was 0 in
	// prod and auto_compact never fired. One durable event per turn, on the
	// non-blocking write-behind path (R6) : zero added latency on the loop.
	e.emitTokenUsage(ctx, in, tr.ID, usage)
	// Signal the background Context Service to recount the EXACT context size
	// now the turn's messages have settled — non-blocking, off the loop.
	e.touchContext(in.SessionID)
	// Final turn-state metrics for the turn_end hook : reflect the
	// post-loop snapshot, the assistant's final content, and the
	// cumulative tool_calls used this turn.
	endMetrics := e.computeHookMetrics(snap, agent, lastContent, toolCallsUsed)
	return &TurnResult{Seq: lastSeq, Content: lastContent}, endMetrics, nil
}

// touchContext fires the non-blocking background recount signal, if wired.
func (e *Engine) touchContext(sessionID string) {
	if e.ContextTouch != nil {
		e.ContextTouch(sessionID)
	}
}

// recordContextParts reports the assembled system prompt + tool schemas to the
// background Context Service for the system/tools breakdown buckets (the
// messages bucket comes from the projection). Non-blocking : it builds small
// string slices and hands off. Only the engine's OWN system prompt goes in the
// system bucket (it is not persisted) ; session system directives stay in the
// messages bucket, so nothing is double-counted.
func (e *Engine) recordContextParts(sessionID, systemPrompt string, tools []llm.ToolSpec) {
	if e.ContextRecordParts == nil {
		return
	}
	var sys []string
	if systemPrompt != "" {
		sys = []string{systemPrompt}
	}
	var toolTexts []string
	for i := range tools {
		if b, err := json.Marshal(tools[i]); err == nil {
			toolTexts = append(toolTexts, string(b))
		}
	}
	e.ContextRecordParts(sessionID, sys, toolTexts)
}

// freshContextView returns the freshest context variable for a session : the
// in-memory ContextView the runtime tracks from the Context Service (it leads
// the durable projection), else one built from the snapshot when nothing is
// tracked yet. This is the value the per-round guard and the hook metrics read,
// so pressure tracks the REAL context even mid-turn.
func (e *Engine) freshContextView(sessionID string, snap sessionstore.SessionSnapshot, brain schema.Brain) contextsvc.ContextView {
	if e.ContextLookup != nil {
		if cv, ok := e.ContextLookup(sessionID); ok && cv.Used > 0 {
			return cv
		}
	}
	return contextsvc.ViewFromSnapshot(snap, brain)
}

// msgByteCap is the per-message snip cap. Normally the fixed maxMessageBytes,
// but tightened to the message token budget (limit − system − tools) when the
// window is small — so a single oversized tool result (a whole-file read) can't
// push the SENT prompt past the window even after compaction keeps it as the
// minimum recent message. ×4 converts the token budget to bytes (chars/token).
func (e *Engine) msgByteCap(sessionID string, snap sessionstore.SessionSnapshot, brain schema.Brain) int {
	capBytes := maxMessageBytes
	cv := e.freshContextView(sessionID, snap, brain)
	if cv.Limit > 0 {
		if budget := cv.Limit - cv.System - cv.Tools - 256; budget > 256 {
			if b := budget * safetyCharsPerToken; b < capBytes {
				capBytes = b
			}
		}
	}
	return capBytes
}

// buildLLMMessages assembles the LLM view from a snapshot : the compacted view
// (when a marker is present) → adapter conversion → per-message snip → the
// engine's fresh system prompt PREPENDED (never replacing a leading durable
// system directive / compaction summary). Used at build AND when the budget
// enforcer rebuilds after a synchronous compaction.
func (e *Engine) buildLLMMessages(ctx context.Context, conv *adapter.Converter, snap sessionstore.SessionSnapshot, systemPrompt, sessionID string, brain schema.Brain) []llm.ChatMessage {
	viewMsgs := snap.Messages
	if cc := snap.ContextCompaction; cc != nil && cc.CutoffSeq > 0 {
		viewMsgs = contextcompact.ApplyView(snap.Messages, cc.CutoffSeq, cc.Summary)
	}
	msgs := conv.Convert(ctx, viewMsgs)
	snipOversizedMessages(msgs, e.msgByteCap(sessionID, snap, brain))
	if systemPrompt != "" {
		msgs = append([]llm.ChatMessage{{Role: "system", Content: systemPrompt}}, msgs...)
	}
	return msgs
}

// estReqTokens is a cheap, conservative size estimate of the EXACT prompt about
// to be sent (messages + tool schemas), via the documented chars/token
// heuristic over the JSON encoding. Used ONLY for the synchronous pre-send
// budget decision — the reported occupancy is always the exact tokenizer count.
// Slightly OVER-estimating (JSON key overhead) is safe : it compacts a touch
// earlier, never later.
func estReqTokens(req *llm.ChatRequest) int {
	chars := 0
	for i := range req.Messages {
		if b, err := json.Marshal(req.Messages[i]); err == nil {
			chars += len(b)
		}
	}
	for i := range req.Tools {
		if b, err := json.Marshal(req.Tools[i]); err == nil {
			chars += len(b)
		}
	}
	// CONSERVATIVE divisor (3, not the ~4 chars/token average) : code & JSON
	// tokenize denser than prose, so chars/4 under-counts and lets a real prompt
	// slip over the window. Over-estimating here only compacts a touch earlier —
	// the SAFE direction. The reported occupancy stays the exact tokenizer count.
	return chars / safetyCharsPerToken
}

// safetyCharsPerToken is the conservative chars/token used for SAFETY decisions
// (the pre-send budget enforcer + the per-message snip) — never for the reported
// value. Lower than the ~4 average so dense code/JSON can't slip over the window.
const safetyCharsPerToken = 3

// enforcePromptBudget is the HARD per-send guarantee : it measures the prompt
// ACTUALLY built for this round (synchronous, exact-input estimate) and, while
// it exceeds the window's usable limit, compacts harder + rebuilds — so a turn
// whose tool results ballooned the prompt can NEVER be sent over the window,
// regardless of how far the async context variable lagged. It always runs (the
// safety net is independent of the configurable auto_compact trigger) and stops
// when under budget OR when a compaction can no longer shrink the prompt (the
// per-message snip already clipped it to fit).
func (e *Engine) enforcePromptBudget(
	ctx context.Context, in TurnInput, agent *schema.Agent, conv *adapter.Converter,
	snap *sessionstore.SessionSnapshot, req *llm.ChatRequest, systemPrompt string,
	pol autoCompactPolicy, keep *int, ratio float64,
) {
	if e.Compactor == nil {
		return
	}
	cv := e.freshContextView(in.SessionID, *snap, agent.Brain)
	limit := cv.Limit
	if limit <= 0 {
		return
	}
	// Target the LOCAL-estimate budget : the model's real count ≈ estimate ×
	// ratio, so to keep real ≤ limit we keep estimate ≤ limit/ratio. ratio is
	// self-calibrated per model from the exact prompt_tokens, so this holds even
	// for Claude/Gemini where no local tokenizer matches.
	if ratio < 1 {
		ratio = defaultEstimateRatio
	}
	effLimit := int(float64(limit) / ratio)
	if effLimit < 256 {
		effLimit = 256
	}
	strategy := pol.strategy
	if strategy == "" {
		strategy = contextcompact.StrategyTruncate
	}
	prev := estReqTokens(req)
	for attempt := 0; attempt < 8 && prev > effLimit; attempt++ {
		k := contextcompact.KeepRecentOrDefault(*keep)
		if err := e.Compactor.CompactSession(ctx, in.SessionID, strategy, k); err != nil {
			return
		}
		if nk := k / 2; nk >= 2 {
			*keep = nk
		} else {
			*keep = 2
		}
		st, serr := e.Sessions.State(in.SessionID)
		if serr != nil || st == nil {
			return
		}
		*snap = st.Snapshot()
		req.Messages = e.buildLLMMessages(ctx, conv, *snap, systemPrompt, in.SessionID, agent.Brain)
		cur := estReqTokens(req)
		if cur >= prev {
			// No more progress (cutoff can't advance / snip already minimal) —
			// the prompt is as small as it gets. Stop ; the emergency-on-overflow
			// path remains the last resort if the provider still rejects.
			e.Logger.Warn("runtime: prompt still over budget after max compaction",
				slog.String("session_id", in.SessionID),
				slog.Int("est", cur), slog.Int("eff_limit", effLimit))
			return
		}
		prev = cur
	}
	if prev > effLimit {
		return
	}
	e.touchContext(in.SessionID)
}

// defaultEstimateRatio is the conservative provider-tokens / local-estimate
// ratio used on a COLD session (turn 1, no calibration yet). Sized for the
// densest case (Claude reading code ≈ 1.6 vs our JSON/3 estimate) so the very
// first prompt is also held under the window ; it self-corrects DOWN after the
// first round for models that tokenize lighter (OpenAI ≈ 1.05).
const defaultEstimateRatio = 1.6

// autoCompactPolicy is the resolved compaction trigger for a turn — the same
// knobs the compiler's _auto_compact hook reads, so the per-round guard stays
// dev-configurable via runtime.context (with brain.context as a fallback).
type autoCompactPolicy struct {
	on        bool
	threshold float64
	keep      int
	strategy  string
}

// resolveAutoCompact mirrors the compiler's injectAutoCompact precedence :
// runtime.context drives, brain.context fills gaps, doc defaults last. on=false
// only when auto_compact is explicitly disabled.
func resolveAutoCompact(rt *schema.RuntimeBlock, brainCtx *schema.ContextConfig) autoCompactPolicy {
	p := autoCompactPolicy{on: true, strategy: contextcompact.StrategyTruncate}
	var rc *schema.ContextConfig
	if rt != nil {
		rc = rt.Context
	}
	if rc != nil {
		if rc.AutoCompact != nil && !*rc.AutoCompact {
			p.on = false
		}
		if rc.CompressionTrigger > 0 {
			p.threshold = rc.CompressionTrigger
		}
		if rc.KeepRecent > 0 {
			p.keep = rc.KeepRecent
		}
		if rc.Strategy != "" {
			p.strategy = string(rc.Strategy)
		}
	}
	if p.threshold == 0 && brainCtx != nil && brainCtx.CompressionTrigger > 0 {
		p.threshold = brainCtx.CompressionTrigger
	}
	if p.keep == 0 && brainCtx != nil && brainCtx.KeepRecent > 0 {
		p.keep = brainCtx.KeepRecent
	}
	if p.threshold == 0 {
		p.threshold = 0.75
	}
	return p
}

// guardContextPressure is the per-ROUND compaction guarantee. Before each LLM
// round it reads the FRESH context variable ; if pressure crosses the app's
// auto_compact threshold it compacts NOW (no cooldown), so a single turn whose
// tool results balloon the context can't blow past the window. *keep is the
// adaptive keep_recent : each fire while still over budget halves it (floor 2)
// so repeated truncation actually converges instead of re-keeping the same huge
// tail. Returns true when it compacted ; the caller's snap is reloaded in place
// so the round rebuilds from the smaller view.
func (e *Engine) guardContextPressure(
	ctx context.Context, in TurnInput, agent *schema.Agent,
	snap *sessionstore.SessionSnapshot, pol autoCompactPolicy, keep *int, lastPromptTokens int,
) bool {
	if !pol.on || e.Compactor == nil || pol.threshold <= 0 {
		return false
	}
	cv := e.freshContextView(in.SessionID, *snap, agent.Brain)
	if cv.Limit <= 0 {
		return false
	}
	// Use the most pessimistic EXACT size : the tracked variable OR the previous
	// round's provider prompt_tokens (exact, synchronous, free). The anchor
	// closes the async gap — if the last prompt actually sent was already over
	// budget, we compact now regardless of whether the recount has landed.
	used := cv.Used
	if lastPromptTokens > used {
		used = lastPromptTokens
	}
	if float64(used)/float64(cv.Limit) < pol.threshold {
		return false
	}
	k := contextcompact.KeepRecentOrDefault(*keep)
	if cerr := e.Compactor.CompactSession(ctx, in.SessionID, pol.strategy, k); cerr != nil {
		e.Logger.Warn("runtime: per-round context guard compaction failed (non-fatal)",
			slog.String("session_id", in.SessionID), slog.String("err", cerr.Error()))
		return false
	}
	if st, serr := e.Sessions.State(in.SessionID); serr == nil && st != nil {
		*snap = st.Snapshot()
	}
	// Halve keep for the next fire so a still-over context drops more next round
	// (truncate with the same keep is idempotent — it would re-keep the same
	// tail). Floor at 2 so we never strand the conversation entirely.
	if nk := k / 2; nk >= 2 {
		*keep = nk
	} else {
		*keep = 2
	}
	// Re-track : tell the service the context shrank so the variable re-converges.
	e.touchContext(in.SessionID)
	e.Logger.Info("runtime: per-round context guard compacted",
		slog.String("session_id", in.SessionID),
		slog.Int("used", used), slog.Int("limit", cv.Limit),
		slog.Int("kept", k))
	return true
}

// emitTokenUsage persists the provider's reported token usage for the turn as
// a durable EventTokenUsage. The projection (a) accumulates TokensIn/TokensOut
// for cost and (b) sets the ContextTokens occupancy gauge (prompt+completion,
// last-wins) that context_pressure reads. Best-effort and non-blocking : a
// failure here must never fail the turn (the usage is also logged).
func (e *Engine) emitTokenUsage(ctx context.Context, in TurnInput, turnID string, usage llm.Usage) {
	if usage.PromptTokens <= 0 && usage.CompletionTokens <= 0 {
		return
	}
	if e.Sessions == nil {
		return
	}
	_, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:          sessionstore.EventTokenUsage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: turnID,
		Cost: &sessionstore.CostPayload{
			TokensIn:         int64(usage.PromptTokens),
			TokensOut:        int64(usage.CompletionTokens),
			CacheReadTokens:  int64(usage.CacheReadTokens),
			CacheWriteTokens: int64(usage.CacheWriteTokens),
		},
	})
	if err != nil {
		e.Logger.Warn("runtime: persist token usage failed (non-fatal)",
			slog.String("session_id", in.SessionID),
			slog.String("err", err.Error()))
	}
}

// persistAssistantStep writes one EventAssistantMessage carrying the
// model's response (text + tool_calls multipart). Returns the seq of
// the persisted event so the caller can correlate.
func (e *Engine) persistAssistantStep(
	ctx context.Context, tr *turn.Turn, in TurnInput, resp *llm.ChatResponse,
) (uint64, error) {
	ev := sessionstore.Event{
		Type:          sessionstore.EventAssistantMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Message: &sessionstore.MessagePayload{
			Role:      "assistant",
			Content:   resp.Content, // legacy back-compat
			Parts:     buildAssistantParts(resp),
			Reasoning: resp.ReasoningContent,
		},
	}
	seq, err := e.Sessions.AppendDurable(ctx, ev)
	if err != nil {
		return 0, fmt.Errorf("runtime: persist assistant message: %w", err)
	}
	return seq, nil
}

// persistInterruptedAssistant durably saves the PARTIAL assistant content that
// was streamed before a user abort. The streamed deltas (EventAssistantDelta)
// are render-only and never projected into the durable message list, so without
// this the partial answer would vanish from history on "stop". Uses a detached,
// time-bounded context so the very cancellation that triggered the interrupt
// can't suppress the save. Best-effort : a failure is logged, never fatal (the
// turn is already unwinding).
// emitTurnError publishes the client-facing `error` event (DaemonError shape)
// for a failed turn. Web/flutter render their error banner from THIS event, not
// from turn_ended{errored} — so without it a real LLM/provider failure never
// surfaces in those UIs, and a turn that died before it ever Started leaves the
// client spinning. Runs ONLY on the failure path, so it adds nothing to a
// successful turn. Best-effort: a failed append is logged, never fatal.
func (e *Engine) emitTurnError(ctx context.Context, in TurnInput, tr *turn.Turn, cause error) {
	info := errclass.Classify(cause)
	retry := info.Retry
	ev := sessionstore.Event{
		Type:          sessionstore.EventError,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Error: &sessionstore.ErrorPayload{
			Error:    info.Error,
			Message:  info.Error,
			Code:     info.Code,
			Category: info.Category,
			Detail:   info.Detail,
			Retry:    &retry,
			Source:   "turn",
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil && e.Logger != nil {
		e.Logger.Warn("runtime: emit turn error event failed",
			slog.String("turn_id", tr.ID), slog.String("err", err.Error()))
	}
}

func (e *Engine) persistInterruptedAssistant(tr *turn.Turn, in TurnInput, content string) {
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev := sessionstore.Event{
		Type:          sessionstore.EventAssistantMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Message: &sessionstore.MessagePayload{
			Role:    "assistant",
			Content: content,
			Parts:   []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: content}},
		},
	}
	if _, err := e.Sessions.AppendDurable(saveCtx, ev); err != nil && e.Logger != nil {
		e.Logger.Warn("runtime: persist interrupted partial failed",
			slog.String("turn_id", tr.ID), slog.String("err", err.Error()))
	}
}

// canonicalizeToolCallNames rewrites tool_call names IN PLACE from the
// OpenAI-conform underscored wire form ("filesystem__read") back to the
// canonical dotted FQN ("filesystem.read") used everywhere internally.
// Called ONCE per LLM response, immediately after the model returns
// tool_calls and BEFORE any persistence / hook / policy stage. Keeping
// the conversion centralized at this single boundary means :
//
//   - Hook conditions written with the natural `filesystem.read`
//     syntax (per docs-site/language/31-tool-hooks.md examples) match
//     regardless of which wire format the model sent.
//   - Persisted EventToolCall / EventToolResult / EventSecurityDecision
//     rows are queryable by canonical FQN. Auditors don't need to know
//     the wire-layer encoding.
//   - The session-projected assistant message (which feeds the next
//     LLM round as tool_call history) round-trips through the planner's
//     sanitizer cleanly — Canonicalize ∘ Sanitize = identity.
//
// Mutation in place is intentional : the wire form has no value to
// anyone past this point, and avoiding the copy spares one allocation
// per turn at the 100K-concurrent-session target.
//
// Idempotent on already-canonical names (no dots → underscored; dots
// present → unchanged), so re-entrant flows (recovery, replays) stay
// safe.
func canonicalizeToolCallNames(calls []llm.ChatToolCall, offered []llm.ToolSpec) {
	// The known canonical FQNs of THIS turn's offered toolset, used to recover a
	// module-less domain action ("read" → "filesystem.read"). This is the
	// top-level, pre-gate1a half of the universal recovery — the dispatcher
	// chokepoint (MetaDispatcher.Dispatch) does the same for meta-routed paths
	// (run_parallel / execute_tool / background_run children), so no path can be
	// denied for a dropped module prefix. The shared resolver is
	// toolname.QualifyBareName.
	known := make([]string, 0, len(offered))
	// singleWire maps a SINGLE-underscore rendering of each offered FQN
	// ("filesystem_read") back to the FQN. Our wire form is double-underscore
	// ("filesystem__read"), but models routinely collapse it to a single `_`,
	// which Canonicalize alone can't recover (a bare `set_goal` is a legit
	// action, so `_`→`.` can't be applied blindly). Keyed on the actually-offered
	// tools the recovery stays unambiguous; a collision drops to "" so it never
	// guesses.
	singleWire := make(map[string]string, len(offered))
	for _, t := range offered {
		fqn := toolname.Canonicalize(t.Name)
		known = append(known, fqn)
		if dot := strings.IndexByte(fqn, '.'); dot >= 0 {
			w := fqn[:dot] + "_" + fqn[dot+1:]
			if prev, seen := singleWire[w]; seen && prev != fqn {
				singleWire[w] = ""
			} else {
				singleWire[w] = fqn
			}
		}
	}
	for i := range calls {
		// Canonicalize the wire form, resolve documented runtime-internal short
		// aliases (Agent → agent_spawn.agent, Remember → memory.remember, …),
		// then recover the module for a still-bare domain action.
		name := toolname.ResolveAlias(toolname.Canonicalize(calls[i].Name))
		name = toolname.QualifyBareName(name, known)
		// Still no module prefix? Recover the single-underscore wire form against
		// the offered set, so `filesystem_read` resolves to `filesystem.read`.
		if !strings.Contains(name, ".") {
			if fqn := singleWire[name]; fqn != "" {
				name = fqn
			}
		}
		calls[i].Name = name
	}
}

// buildMiddlewareContext snapshots the outbound request into the middleware
// context : the base system prompt (kept separate so a per-round injection
// never accumulates onto the durable prompt) and the messages about to be sent.
func buildMiddlewareContext(agent *schema.Agent, in TurnInput, turn int, systemPrompt string, msgs []llm.ChatMessage) *ports.MiddlewareContext {
	agentID := ""
	if agent != nil {
		agentID = agent.ID
	}
	pm := make([]ports.LLMMessage, len(msgs))
	for i := range msgs {
		pm[i] = ports.LLMMessage{Role: msgs[i].Role, Content: msgs[i].Content}
	}
	return &ports.MiddlewareContext{
		AgentID: agentID, SessionID: in.SessionID, UserID: in.UserID,
		Turn: turn, SystemPrompt: systemPrompt, Messages: pm, Metadata: map[string]any{},
	}
}

// applyMiddlewareContext writes the Before mutations back onto the outbound
// request : per-message content edits (mask_secrets), then the injected system
// prompt onto the leading system message (prompt_inject / rag_inject).
func applyMiddlewareContext(msgs []llm.ChatMessage, mctx *ports.MiddlewareContext, systemPrompt string) {
	n := len(msgs)
	if len(mctx.Messages) < n {
		n = len(mctx.Messages)
	}
	for i := 0; i < n; i++ {
		msgs[i].Content = mctx.Messages[i].Content
	}
	if systemPrompt != "" && len(msgs) > 0 && msgs[0].Role == "system" {
		msgs[0].Content = mctx.SystemPrompt
	}
}

// toPortsToolCalls adapts the LLM tool calls into the middleware port type
// (arguments JSON-encoded), so After hooks can inspect them.
func toPortsToolCalls(calls []llm.ChatToolCall) []ports.LLMToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ports.LLMToolCall, len(calls))
	for i := range calls {
		args := ""
		if calls[i].Arguments != nil {
			if b, err := json.Marshal(calls[i].Arguments); err == nil {
				args = string(b)
			}
		}
		out[i] = ports.LLMToolCall{ID: calls[i].ID, Name: calls[i].Name, Arguments: args}
	}
	return out
}

// persistToolCallEvents writes one EventToolCall per LLM-returned call,
// status="pending". Used by the timeline so each call appears
// individually. Errors on persist are logged ; we don't abort the
// loop because the assistant message already records the same
// intent in its multipart parts.
func (e *Engine) persistToolCallEvents(
	ctx context.Context, tr *turn.Turn, in TurnInput, calls []llm.ChatToolCall,
) {
	for _, tc := range calls {
		callEv := sessionstore.Event{
			Type:          sessionstore.EventToolCall,
			SessionID:     in.SessionID,
			AppID:         in.AppID,
			UserID:        in.UserID,
			CorrelationID: tr.ID,
			Tool: &sessionstore.ToolPayload{
				CallID:    tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
				Status:    "pending",
			},
		}
		if _, err := e.Sessions.AppendDurable(ctx, callEv); err != nil {
			e.Logger.Warn("runtime: persist tool_call event failed",
				slog.String("call_id", tc.ID),
				slog.String("err", err.Error()))
		}
	}
}

// dispatchToolsParallel fans out one goroutine per tool call and waits
// for all of them via a WaitGroup. The dispatcher's contract requires
// concurrent-safety so we don't need any synchronisation here beyond
// the WaitGroup.
//
// CRUCIAL ISOLATION POINT : the turn's goroutine blocks on the
// WaitGroup, but OTHER sessions' turns are not affected. They have
// their own Engine.Run goroutines, their own dispatcher invocations.
// A 30s tool call in session A is invisible to session B.
func (e *Engine) dispatchToolsParallel(
	ctx context.Context, tr *turn.Turn, in TurnInput,
	app *appmgr.RuntimeApp, agent *schema.Agent,
	calls []llm.ChatToolCall, metrics hooks.Payload, gate *modeGuard,
	beBlocks map[int]string,
) []ToolOutcome {
	// Carry the mode guard down the dispatch ctx so the shared sub-tool
	// chokepoint (enforceGate) enforces the mode allow-list on tools reached
	// via meta paths (execute_tool / run_parallel / background_run), closing
	// the bypass where a meta-tool could invoke a mode-blocked sub-tool.
	ctx = withModeGuard(ctx, gate)
	// WD : attach the session's workdir PathPolicy to the dispatch ctx so the
	// same chokepoint confines every path-typed arg to the workdir — top-level
	// AND meta paths. Resolved once per turn ; absent for apps with no workdir.
	if e.PathPolicies != nil {
		if pp, ok := e.PathPolicies.PathPolicyFor(in.AppID, in.SessionID); ok {
			ctx = workdir.WithPathPolicy(ctx, pp)
		}
	}
	outcomes := make([]ToolOutcome, len(calls))
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, tc := range calls {
		go func(i int, tc llm.ChatToolCall) {
			defer wg.Done()
			start := time.Now()
			// Last-resort panic shield. MetaDispatcher.Dispatch already recovers
			// module panics, but enforceGate / gates / approval run OUTSIDE it in
			// this goroutine, and an unrecovered panic in ANY goroutine takes the
			// whole daemon down. Degrade to an errored outcome so every other
			// session keeps running.
			defer func() {
				if r := recover(); r != nil {
					outcomes[i] = ToolOutcome{
						Status:     "errored",
						Error:      "tool=" + tc.Name + ": " + safego.Report("engine.tool:"+tc.Name, r),
						DurationMs: time.Since(start).Milliseconds(),
					}
				}
			}()

			// Behavioral block (decided in the sequential pre-pass) : the call
			// must NOT execute. Its violation message is the error result, and
			// the post-pass skips it (state already reflects only ran tools).
			if beBlocks != nil {
				if msg, blocked := beBlocks[i]; blocked {
					outcomes[i] = ToolOutcome{
						Status:     "errored",
						Error:      msg,
						DurationMs: time.Since(start).Milliseconds(),
					}
					return
				}
			}

			// Composer-mode guard (defense in depth) : the offered tool list
			// already excludes blocked tools, but the model may hallucinate a
			// tool it remembers from another mode. Reject it with an explicit,
			// non-retryable error so the agent asks the user to switch mode.
			if gate != nil {
				if _, ok := gate.allowed[tc.Name]; !ok {
					outcomes[i] = ToolOutcome{
						Status: "errored",
						Error: fmt.Sprintf(
							"Tool %q is blocked in mode %q. Allowed tools: %s. Ask the user to switch to a mode that allows this tool. Do not retry this call.",
							tc.Name, gate.label, gate.allowedList),
						DurationMs: time.Since(start).Milliseconds(),
					}
					return
				}
			}

			// SG-4 (top-level path) : gate the LLM-emitted call before
			// dispatch. enforceGate is the single gate definition shared
			// with the re-entrant meta paths (execute_tool / run_parallel
			// / background_run), so every real sub-tool is evaluated
			// exactly once. Meta-tool wrappers bypass here and their
			// resolved target is gated below in the dispatcher. tr/&in
			// enable the turn-scoped approval hook + phase transitions.
			if blocked := e.enforceGate(ctx, in.SessionID, in.AppID, in.UserID, tr.ID, app, agent, tc.Name, tc.ID, tc.Arguments, tr, &in); blocked != nil {
				blocked.DurationMs = time.Since(start).Milliseconds()
				outcomes[i] = *blocked
				return
			}

			agentID := ""
			if agent != nil {
				agentID = agent.ID
			}
			// RT-4 : fire canonical tool_start (alias pre_tool_use
			// resolves to same event). Hook actions may VETO the
			// call via the `gate` action — fireHook returns true
			// when allowed.
			gateRes := e.fireHookGate(ctx, in.AppID, agent, schema.HookEventToolStart, withTurnState(hooks.Payload{
				AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID,
				TurnID: tr.ID, ToolName: tc.Name, ToolArgs: tc.Arguments,
			}, metrics))
			if gateRes != nil && !gateRes.Allow {
				outcomes[i] = ToolOutcome{
					Status:     "errored",
					Error:      "blocked by hook gate: " + gateRes.Reason,
					DurationMs: time.Since(start).Milliseconds(),
				}
				return
			}
			// Bound this single dispatch. Two regimes :
			//
			//   - long/human tools (ask_user blocks on a person ; run_parallel /
			//     use_skill / call_app / agent_spawn run whole sub-flows) get NO
			//     per-call cap — they have their own bounds — but a keepalive
			//     ticker pings the turn watchdog while they run, so a legitimate
			//     long wait is never mistaken for a stalled turn.
			//   - every other (leaf) tool gets the per-call timeout : one
			//     slow/hung tool can't eat the turn. On timeout the ctx is
			//     cancelled and we synthesise a clear errored outcome the model
			//     reacts to ; the loop continues to the next round. The cap is
			//     shorter than the idle watchdog so a capped tool always returns
			//     before the turn could be judged stalled.
			dctx := ctx
			var toolCancel context.CancelFunc
			var stopTicker chan struct{}
			if isLongRunningTool(tc.Name) {
				stopTicker = make(chan struct{})
				go keepaliveTicker(ctx, stopTicker)
			} else if e.ToolTimeout > 0 {
				dctx, toolCancel = context.WithTimeout(ctx, e.ToolTimeout)
			}
			outcomes[i] = e.dispatcher().Dispatch(dctx, ToolInvocation{
				CallID:     tc.ID,
				Name:       tc.Name,
				Args:       tc.Arguments,
				AppID:      in.AppID,
				AgentID:    agentID,
				AgentRunID: agentRunID(in.AgentRunID, agentID),
				UserID:     in.UserID,
				SessionID:  in.SessionID,
				UserJWT:    in.UserJWT,
			})
			if stopTicker != nil {
				close(stopTicker)
			}
			// Only the per-tool deadline counts as a timeout here — a parent
			// cancellation (client abort / turn idle cutoff) is handled by the
			// loop, not turned into a tool error.
			if toolCancel != nil {
				timedOut := dctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
				toolCancel()
				if timedOut && outcomes[i].Status != "errored" {
					outcomes[i] = ToolOutcome{
						Status: "errored",
						Error: fmt.Sprintf(
							"tool %q exceeded the %s per-call time limit and was cancelled; narrow the operation (smaller scope / fewer files) or run it via background_run",
							tc.Name, e.ToolTimeout),
						DurationMs: time.Since(start).Milliseconds(),
					}
				}
			}
			if outcomes[i].DurationMs == 0 {
				outcomes[i].DurationMs = time.Since(start).Milliseconds()
			}
			if outcomes[i].Status == "" {
				// Defensive : dispatcher should set this but we don't
				// trust the contract blindly.
				if outcomes[i].Error != "" {
					outcomes[i].Status = "errored"
				} else {
					outcomes[i].Status = "completed"
				}
			}
			// RT-4 : fire canonical tool_end (alias post_tool_use). A
			// transform_result hook mutates resultMap IN PLACE ; when
			// it does (res.Modified) we re-project the change back onto
			// the outcome so the agent actually sees the transformed
			// result. An inject_message hook queues a next-round message.
			resultMap := outcomeToResult(outcomes[i])
			res := e.fireHook(ctx, in.AppID, agent, schema.HookEventToolEnd, withTurnState(hooks.Payload{
				AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID,
				TurnID: tr.ID, ToolName: tc.Name, ToolArgs: tc.Arguments,
				ToolStatus: outcomes[i].Status, ToolError: outcomes[i].Error,
				ToolResult: resultMap,
			}, metrics))
			if res.Modified {
				applyResultMutation(&outcomes[i], resultMap)
			}
			if len(res.Injects) > 0 {
				e.applyInjections(ctx, in, tr, res.Injects)
			}
		}(i, tc)
	}
	wg.Wait()
	return outcomes
}

// runBehaviorClassifier runs the pre-turn semantic classifier and, when it
// produces a directive, persists it durably + re-snapshots so round 1 sees it.
// Every failure path inside Classify returns "" — this never breaks the turn.
func (e *Engine) runBehaviorClassifier(
	ctx context.Context, tr *turn.Turn, app *appmgr.RuntimeApp, agent *schema.Agent,
	be *behavior.Engine, in TurnInput, snap *sessionstore.SessionSnapshot,
	tools []llm.ToolSpec, apiKey, baseURL string,
) {
	cctx := ctx
	if to := be.ClassifierTimeout(); to > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(to)*time.Second)
		defer cancel()
	}
	chat := func(c context.Context, system, user string) (string, error) {
		resp, err := e.LLM.Chat(c, &llm.ChatRequest{
			BYOK:     app.Meta != nil && app.Meta.BYOK,
			Provider: agent.Brain.Provider,
			Model:    agent.Brain.Model,
			APIKey:   apiKey,
			BaseURL:  baseURL,
			UserJWT:  in.UserJWT,
			Messages: []llm.ChatMessage{
				{Role: "system", Content: system},
				{Role: "user", Content: user},
			},
		})
		if err != nil || resp == nil {
			return "", err
		}
		return resp.Content, nil
	}
	directive := be.Classify(cctx, in.SessionID, classifyInputFromSnap(*snap, tools), chat)
	if directive == "" {
		return
	}
	e.injectSystemDirective(ctx, in, tr.ID, directive, DirectiveBehaviorClassify, nil, nil)
	if st, err := e.Sessions.State(in.SessionID); err == nil && st != nil {
		*snap = st.Snapshot()
	}
}

// classifyInputFromSnap derives the classifier's per-turn context from the
// session snapshot + offered tools : the latest user message, the tool
// inventory, and the recent conversation.
func classifyInputFromSnap(snap sessionstore.SessionSnapshot, tools []llm.ToolSpec) behavior.ClassifyInput {
	in := behavior.ClassifyInput{}
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if snap.Messages[i].Role == "user" {
			in.UserMessage = snap.Messages[i].Content
			break
		}
	}
	for _, t := range tools {
		in.ToolInventory = append(in.ToolInventory, behavior.ToolInfo{
			Name: t.Name, Description: t.Description,
		})
	}
	for _, m := range snap.Messages {
		in.Recent = append(in.Recent, behavior.HistMsg{Role: m.Role, Content: m.Content})
	}
	return in
}

// injectBehaviorNotes persists this round's accumulated behavior warn/remind
// violations as one durable system directive (role=system), so the model sees
// the guidance on the next round. No-op when there are none.
func (e *Engine) injectBehaviorNotes(ctx context.Context, in TurnInput, tr *turn.Turn, notes []behavior.Violation) {
	if len(notes) == 0 {
		return
	}
	parts := make([]string, 0, len(notes))
	severity := "warning"
	for _, v := range notes {
		parts = append(parts, v.Format())
		if v.Level == "block" {
			severity = "critical"
		}
	}
	// Ride the authoritative directive protocol : a plain "[BEHAVIOR WARNING]"
	// system message reads as a suggestion the model ignores ; the envelope
	// makes it binding (sysAuthorityPreamble).
	body := wrapRuntimeDirective("behavior_enforcement", severity, strings.Join(parts, "\n\n"))
	e.injectSystemDirective(ctx, in, tr.ID, body, DirectiveBehaviorEnforce, nil, nil)
}

// emitSecurityDecision writes the documented audit row (SG-6) for
// one policy evaluation. No-op when the decision came from a bypass
// branch (system module or meta-tool) — per the SG-6 design those
// don't need an audit trail.
//
// Errors from AppendDurable are logged at warn level but NOT
// propagated : the security decision has already been made and
// applied ; losing an audit row should not abort the turn (better
// to have a missed log than a wedged session).
func (e *Engine) emitSecurityDecision(
	ctx context.Context,
	sessionID, appID, userID, correlationID string,
	agent *schema.Agent, module, action string,
	params map[string]any, d policy.Decision,
) {
	gate := string(d.Gate)
	if gate == "system_module_bypass" || gate == "meta_tool_bypass" {
		return // bypass = no audit row
	}
	agentIDStr := ""
	if agent != nil {
		agentIDStr = agent.ID
	}
	riskLevel := ""
	if e.PolicyEvaluator != nil {
		if dp, ok := e.PolicyEvaluator.(*DefaultPolicyEvaluator); ok && dp.Lookup != nil {
			if spec := dp.Lookup.LookupToolSpec(module, action); spec != nil {
				riskLevel = string(spec.RiskLevel)
			}
		}
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventSecurityDecision,
		SessionID:     sessionID,
		AppID:         appID,
		UserID:        userID,
		CorrelationID: correlationID,
		Security: &sessionstore.SecurityDecisionPayload{
			AppID:          appID,
			AgentID:        agentIDStr,
			SessionID:      sessionID,
			UserID:         userID,
			Module:         module,
			Action:         action,
			RiskLevel:      riskLevel,
			ParamsRedacted: policy.RedactParams(params, nil),
			Decision:       d.Kind.String(),
			Gate:           gate,
			Reason:         d.Reason,
			Caller:         policy.CallerLLM.String(),
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil {
		e.Logger.Warn("runtime: failed to emit security_decision audit row",
			slog.String("module", module),
			slog.String("action", action),
			slog.String("decision", d.Kind.String()),
			slog.String("err", err.Error()))
	}
}

// awaitApproval implements the documented synchronous-pause flow
// from security-01-approval.md :
//
//  1. Generate a request_id for the approval.
//  2. Emit EventApprovalRequest with the full payload (request_id,
//     agent_id, user_id, app_id, session_id, tool_name, tool_params,
//     risk_level, reason).
//  3. Transition the turn to PhaseWaitingApproval (best-effort —
//     phase changes are observational, not enforced).
//  4. Block on ApprovalRegistry.Wait until a human resolution
//     arrives, the configured approval_timeout fires, or ctx is
//     cancelled.
//  5. Emit the matching event (EventApprovalGranted on approve,
//     EventApprovalDenied on deny / timeout / cancel).
//  6. Transition back to PhaseRunning.
//
// Returns the Resolution so the caller can map it into a ToolOutcome.
// If no ApprovalRegistry is wired (test / dev mode), returns
// ResultCancelled with an explanatory reason.
// tr and in are OPTIONAL : non-nil on the top-level dispatch path
// (enables the approval_request hook, message injections, and the
// observational phase transitions) ; nil on the re-entrant chokepoint
// path (execute_tool / run_parallel / background_run), where the
// durable approval_request event + wait + result still fire so the
// CLI modal works, but the turn-scoped extras are skipped.
func (e *Engine) awaitApproval(
	ctx context.Context,
	sessionID, appID, userID, correlationID string,
	app *appmgr.RuntimeApp, agent *schema.Agent,
	toolName, callID, module, action string,
	params map[string]any, reason string,
	tr *turn.Turn, in *TurnInput,
) approval.Resolution {
	if e.ApprovalRegistry == nil {
		return approval.Resolution{
			Result: approval.ResultCancelled,
			Reason: "no approval registry wired (SG-5 disabled at runtime)",
		}
	}

	requestID := uuid.NewString()
	agentIDStr := ""
	if agent != nil {
		agentIDStr = agent.ID
	}
	riskLevel := ""
	if e.PolicyEvaluator != nil {
		// Best-effort risk_level extraction via the same lookup the
		// gates used. nil-safe : an unknown action lands as "".
		if d, ok := e.PolicyEvaluator.(*DefaultPolicyEvaluator); ok && d.Lookup != nil {
			if spec := d.Lookup.LookupToolSpec(module, action); spec != nil {
				riskLevel = string(spec.RiskLevel)
			}
		}
	}

	// Arm the waiter BEFORE emitting the request event. A fast client can
	// observe the durable EventApprovalRequest and POST /approve before this
	// goroutine reaches Wait ; arming first guarantees Resolve always finds the
	// waiter, so the approval can never be lost to the emit-before-wait race.
	pending := e.ApprovalRegistry.Arm(requestID)

	// Step 2 — emit the documented EventApprovalRequest payload.
	_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:          sessionstore.EventApprovalRequest,
		SessionID:     sessionID,
		AppID:         appID,
		UserID:        userID,
		CorrelationID: correlationID,
		Approval: &sessionstore.ApprovalPayload{
			ID:         requestID,
			Kind:       "tool_call",
			Status:     "pending",
			Reason:     reason,
			AgentID:    agentIDStr,
			ToolName:   toolName,
			ToolParams: params,
			RiskLevel:  riskLevel,
			CallID:     callID,
		},
	})

	// approval_request hook + injections + phase transition are
	// turn-scoped : only on the top-level path (tr/in non-nil). The
	// re-entrant chokepoint path skips them — the durable event above
	// is what the CLI modal reacts to.
	if tr != nil && in != nil {
		arRes := e.fireHook(ctx, appID, agent, schema.HookEventApprovalRequest, hooks.Payload{
			AppID: appID, SessionID: sessionID, UserID: userID,
			TurnID: correlationID, ToolName: toolName, ToolArgs: params,
		})
		e.applyInjections(ctx, *in, tr, arRes.Injects)
		_ = tr.TransitionTo(ctx, turn.PhaseWaitingApproval)
	}

	// Step 4 — block on the registry. Timeout from app capabilities
	// (range [30, 3600], default 300) per security-01-approval.md.
	//
	// Waiting for a HUMAN to approve is NOT a stalled turn — so ping the idle
	// watchdog for the whole wait, exactly as a long-running/human-in-the-loop
	// tool does. Without this the session runner's idle cutoff (5 min) fired
	// first and the user saw a confusing "approval cancelled: context canceled"
	// instead of a patient wait bounded by the approval's OWN timeout (which a
	// 5-min-equal default made a dead heat the watchdog won). The approval
	// timeout — not the watchdog — is what must bound a human's think time.
	timeout := approvalTimeout(app)
	stopTicker := make(chan struct{})
	go keepaliveTicker(ctx, stopTicker)
	res := pending.Wait(ctx, timeout)
	close(stopTicker)

	// Step 5 — emit the matching event so the timeline / audit log
	// reflects the resolution durably. The HTTP /approve endpoint
	// also emits EventApprovalGranted/Denied when a human resolved
	// the request ; we emit the same event types here for timeout
	// and cancel (which never traverse the HTTP path).
	var resultEvent sessionstore.EventType
	resultStatus := "denied"
	switch res.Result {
	case approval.ResultApproved:
		resultEvent = sessionstore.EventApprovalGranted
		resultStatus = "granted"
	case approval.ResultDenied:
		resultEvent = sessionstore.EventApprovalDenied
	case approval.ResultTimeout:
		// Doc : "the daemon emits an auto_denied event once the
		// deadline lapses". We use EventApprovalDenied with
		// status="auto_denied" to keep the schema simple.
		resultEvent = sessionstore.EventApprovalDenied
		resultStatus = "auto_denied"
	case approval.ResultCancelled:
		resultEvent = sessionstore.EventApprovalDenied
		resultStatus = "cancelled"
	}
	// For approve/deny coming from HTTP, the handler ALSO emits the
	// event ; we emit our own here so the timeline records both the
	// "resolution received by the runtime" moment and the "resolution
	// applied by the HTTP layer" moment. They will carry the same
	// request_id, so projection deduplicates by ID downstream.
	if res.Result == approval.ResultTimeout || res.Result == approval.ResultCancelled {
		_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
			Type:          resultEvent,
			SessionID:     sessionID,
			AppID:         appID,
			UserID:        userID,
			CorrelationID: correlationID,
			Approval: &sessionstore.ApprovalPayload{
				ID:     requestID,
				Status: resultStatus,
				Reason: res.Reason,
			},
		})
	}

	// Step 6 — phase back to running (turn-scoped, top-level only).
	if tr != nil {
		_ = tr.TransitionTo(ctx, turn.PhaseRunning)
	}

	return res
}

// enforceGate runs SG-4 for one resolved sub-tool. Returns nil to let
// the call proceed, or an errored outcome to short-circuit (deny, or
// approval refused / timed out / cancelled). This is the SINGLE gate
// definition shared by the top-level dispatch and the re-entrant meta
// paths, so every real sub-tool is evaluated exactly once at the point
// its name is resolved. NeedsApproval blocks here until resolved. tr/in
// are non-nil only on the top-level path (see awaitApproval).
func (e *Engine) enforceGate(
	ctx context.Context,
	sessionID, appID, userID, correlationID string,
	app *appmgr.RuntimeApp, agent *schema.Agent,
	toolName, callID string, args map[string]any,
	tr *turn.Turn, in *TurnInput,
) *ToolOutcome {
	// Heal common LLM arg-shape mistakes (a value keyed under the tool's own
	// name, or one unambiguous stray key) at THIS single chokepoint — before the
	// gates, the workdir path enforcement, and the dispatch — so the cure is
	// schema-true and covers top-level AND meta paths alike. Schema-driven and
	// conservative : it only fills a still-empty required string param, never
	// overwrites or invents. Kills the whole class of "tools work randomly"
	// failures whose real cause was the model naming an argument wrong.
	if hm, ha := splitToolName(toolName); args != nil {
		if spec := e.toolSpec(hm, ha); spec != nil {
			if healToolArgs(spec, toolName, args) && e.Logger != nil {
				e.Logger.Debug("runtime: healed tool args to match schema", "tool", toolName)
			}
		}
	}
	// Composer-mode allow-list, enforced at the SINGLE chokepoint so it covers
	// tools reached through meta paths (execute_tool / run_parallel /
	// background_run), not just top-level calls. Runs BEFORE the policy
	// evaluator nil-check so a mode still blocks even when no security policy
	// is wired. Meta-tools themselves are in the allow-list (always granted),
	// so only the resolved domain sub-tool can trip this.
	if g := modeGuardFromCtx(ctx); g.blocks(toolname.Canonicalize(toolName)) {
		return &ToolOutcome{Status: "errored", Error: g.blockedError(toolName)}
	}
	// Behavioral block enforcement on the RE-ENTRANT (meta) path only —
	// tr/in are nil there. The top-level path already ran the behavior
	// pre-pass in runPhases. Uses the READ-ONLY BlockedSubTool so it is safe
	// under run_parallel's concurrent sub-tool dispatch (no state mutation).
	// Without this, a behavior `action: block` rule could be bypassed by
	// routing the sub-tool through execute_tool / run_parallel.
	if tr == nil && in == nil {
		if be := e.behaviorFor(app); be != nil {
			if v := be.BlockedSubTool(sessionID, toolName, args); v != nil {
				return &ToolOutcome{Status: "errored", Error: v.Format()}
			}
		}
	}
	// WD : workdir confinement. When the session has a PathPolicy on ctx,
	// every path-typed arg of the resolved tool is rewritten to the enforced
	// absolute path or the call is rejected — at this single chokepoint, so it
	// covers top-level AND meta paths (execute_tool / run_parallel / background)
	// and no module can forget it. Runs before the gates : a path escape is a
	// hard block regardless of the capability policy.
	if pp, ok := workdir.PathPolicyFromContext(ctx); ok {
		module, action := splitToolName(toolName)
		if keys := e.pathParamNames(module, action); len(keys) > 0 {
			if err := workdir.EnforceArgs(pp, args, keys...); err != nil {
				return &ToolOutcome{Status: "errored", Error: "denied by workdir policy: " + err.Error()}
			}
		}
	}
	if e.PolicyEvaluator == nil {
		return nil
	}
	module, action := splitToolName(toolName)
	d := e.PolicyEvaluator.Evaluate(ctx, EvaluateInput{
		AppID:     appID,
		SessionID: sessionID,
		UserID:    userID,
		Module:    module,
		Action:    action,
		App:       app,
		Agent:     agent,
	})
	e.emitSecurityDecision(ctx, sessionID, appID, userID, correlationID, agent, module, action, args, d)
	switch d.Kind {
	case policy.DecisionDeny:
		return &ToolOutcome{
			Status: "errored",
			Error:  "denied by security policy (" + string(d.Gate) + "): " + d.Reason,
		}
	case policy.DecisionNeedsApproval:
		res := e.awaitApproval(ctx, sessionID, appID, userID, correlationID, app, agent, toolName, callID, module, action, args, d.Reason, tr, in)
		switch res.Result {
		case approval.ResultApproved:
			return nil
		case approval.ResultDenied:
			return &ToolOutcome{Status: "errored", Error: "approval denied: " + res.Reason}
		case approval.ResultTimeout:
			return &ToolOutcome{Status: "errored", Error: "approval timed out: " + res.Reason}
		case approval.ResultCancelled:
			return &ToolOutcome{Status: "errored", Error: "approval cancelled: " + res.Reason}
		}
	}
	return nil
}

// pathParamNames returns the path-typed parameter names of (module, action),
// resolved from the same ToolSpecLookup the gates use. Empty when no spec is
// known (meta-tools, or no lookup wired) → the workdir enforcement is skipped
// because there's nothing to identify as a path.
func (e *Engine) pathParamNames(module, action string) []string {
	if spec := e.toolSpec(module, action); spec != nil {
		return spec.PathParamNames()
	}
	return nil
}

// toolSpec resolves the declared spec for (module, action) via the same lookup
// the gates use, or nil when none is known (meta-tools, or no lookup wired).
func (e *Engine) toolSpec(module, action string) *tool.Spec {
	if dp, ok := e.PolicyEvaluator.(*DefaultPolicyEvaluator); ok && dp.Lookup != nil {
		return dp.Lookup.LookupToolSpec(module, action)
	}
	return nil
}

// GateSubTool is the chokepoint hook the MetaDispatcher calls before
// executing a domain tool reached via a meta path (execute_tool,
// run_parallel, background_run launch). It resolves app+agent from the
// invocation's routing ids, then runs the same gate as a direct call,
// so capabilities.deny / approve apply no matter how the model reached
// the sub-tool. tr/in are nil here (re-entrant path).
func (e *Engine) GateSubTool(ctx context.Context, inv ToolInvocation) *ToolOutcome {
	app, agent := e.resolveAppAgent(ctx, inv.AppID, inv.AgentID)
	return e.enforceGate(ctx, inv.SessionID, inv.AppID, inv.UserID, inv.CallID, app, agent, inv.Name, inv.CallID, inv.Args, nil, nil)
}

// resolveAppAgent looks up the RuntimeApp and the named agent from the
// app manager. nil-safe : (nil, nil) when the app can't be resolved,
// (app, nil) when the agent id doesn't match — the gates then fail
// closed via the inactive-app / nil-spec paths.
func (e *Engine) resolveAppAgent(ctx context.Context, appID, agentID string) (*appmgr.RuntimeApp, *schema.Agent) {
	if e.Apps == nil {
		return nil, nil
	}
	app, err := e.Apps.Get(ctx, appID)
	if err != nil || app == nil || app.Definition == nil {
		return app, nil
	}
	for i := range app.Definition.Agents {
		if app.Definition.Agents[i].ID == agentID {
			return app, &app.Definition.Agents[i]
		}
	}
	return app, nil
}

// approvalTimeout resolves the effective approval timeout for the
// given app. Doc rules (security-01-approval.md) :
//
//   - range [30, 3600] seconds
//   - default 300 (5 minutes)
//   - configured via tools.capabilities.approval_timeout
func approvalTimeout(app *appmgr.RuntimeApp) time.Duration {
	const defaultS = 300
	const minS = 30
	const maxS = 3600
	v := defaultS
	if app != nil && app.Definition != nil && app.Definition.Tools != nil &&
		app.Definition.Tools.Capabilities != nil {
		if t := app.Definition.Tools.Capabilities.ApprovalTimeout; t > 0 {
			v = t
		}
	}
	if v < minS {
		v = minS
	}
	if v > maxS {
		v = maxS
	}
	return time.Duration(v) * time.Second
}

// splitToolName splits the LLM-emitted tool name into (module, action).
// Convention used by the doc + the existing tool catalog : the first
// "." separates the module from the action. Meta-tools (execute_tool,
// search_tools, ...) come without a module prefix ; we return module=""
// in that case and let policy.RunGates apply the meta-tool bypass.
func splitToolName(name string) (module, action string) {
	// Canonicalise first : the security gate and the workdir enforcer split on
	// the dotted FQN, but a model may emit the wire (`__`) or `::` form, and a
	// re-entrant meta call (execute_tool(name="filesystem::glob")) can deliver a
	// raw name here without passing the inbound canonicaliser. Splitting a
	// non-dotted name would yield module="" and silently deny every such call at
	// gate1a — so normalise at the chokepoint, no name form can fool the gate.
	name = toolname.Canonicalize(name)
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i], name[i+1:]
		}
	}
	return "", name
}

// persistToolResults writes one EventToolResult per outcome. The
// projection appends a "tool" role Message into session state so the
// next iteration's MessagesToLLM call sees the results.
func (e *Engine) persistToolResults(
	ctx context.Context, tr *turn.Turn, in TurnInput,
	calls []llm.ChatToolCall, outcomes []ToolOutcome,
) error {
	// Tool results are REQUIRED for tool_call/result pairing in every future
	// prompt. If the turn's ctx is already cancelled (user abort while tools were
	// running), persisting with it would fail and leave the assistant's
	// tool_calls dangling on resume. Fall back to a detached, time-bounded ctx so
	// the outcomes — even the "context canceled" errored ones — still land and
	// the durable history stays self-consistent.
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	evs := make([]sessionstore.Event, len(calls))
	for i, tc := range calls {
		out := outcomes[i]
		var output any
		if len(out.Parts) == 0 {
			// Legacy fallback : collapse text-only outcomes into
			// Tool.Output for back-compat with older consumers.
			output = ""
		}
		tp := &sessionstore.ToolPayload{
			CallID:     tc.ID,
			Name:       tc.Name,
			Status:     out.Status,
			Parts:      out.Parts,
			Output:     output,
			Error:      out.Error,
			DurationMs: out.DurationMs,
			Metadata:   out.Metadata, // client-only side-channel ; never reaches the LLM
		}
		// Flatten the client-only diff view onto the wire's top-level fields
		// (diff / unified_diff / previous_content / new_content), matching the
		// legacy daemon so existing clients render insertions/deletions. The LLM
		// projection reads only Parts/Output, so it never sees any of this.
		if d := out.Diff; d != nil {
			tp.Diff = d.Summary
			tp.UnifiedDiff = d.Unified
			tp.PreviousContent = d.PreviousContent
			tp.NewContent = d.NewContent
			if d.Additions > 0 || d.Deletions > 0 {
				if tp.Metadata == nil {
					tp.Metadata = map[string]any{}
				}
				tp.Metadata["additions"] = d.Additions
				tp.Metadata["deletions"] = d.Deletions
			}
		}
		evs[i] = sessionstore.Event{
			Type:          sessionstore.EventToolResult,
			SessionID:     in.SessionID,
			AppID:         in.AppID,
			UserID:        in.UserID,
			CorrelationID: tr.ID,
			Tool:          tp,
		}
	}

	// Many tools finished in parallel : group-commit their results so the
	// round pays ~one fsync instead of N. The single-tool case keeps the
	// plain durable append (cheaper than a one-element batch). A session
	// store that doesn't implement durableBatcher (test mocks) falls back
	// to the serial loop with identical semantics.
	if len(evs) > 1 {
		if batcher, ok := e.Sessions.(durableBatcher); ok {
			if _, err := batcher.AppendDurableBatch(ctx, evs); err != nil {
				return fmt.Errorf("runtime: persist %d tool_results: %w", len(evs), err)
			}
			// Tool results changed the context : refresh the gauge NOW (per-session,
			// non-blocking) instead of waiting for the next round's build. in.SessionID
			// is this turn's own session — a sub-agent's sub-session is distinct, so
			// this never touches another session.
			e.touchContext(in.SessionID)
			return nil
		}
	}
	for i := range evs {
		if _, err := e.Sessions.AppendDurable(ctx, evs[i]); err != nil {
			return fmt.Errorf("runtime: persist tool_result %q: %w", calls[i].ID, err)
		}
	}
	e.touchContext(in.SessionID) // tool results landed → refresh the gauge now (per-session)
	return nil
}

// durableBatcher is the optional group-commit capability of the session
// store. *sessionstore.Bus implements it; mocks need not.
type durableBatcher interface {
	AppendDurableBatch(ctx context.Context, evs []sessionstore.Event) ([]uint64, error)
}

// dispatcher returns the wired dispatcher, falling back to the noop
// dispatcher when none is configured. Same nil-safety pattern as tools().
func (e *Engine) dispatcher() ToolDispatcher {
	if e.Dispatcher == nil {
		return NoopDispatcher{}
	}
	return e.Dispatcher
}

// outcomeToResult projects a ToolOutcome into the map shape the
// hooks Payload exposes as `{{tool.result.X}}` placeholders. For
// V1 the projection is intentionally narrow : success/error +
// joined text content. Multi-part outcomes get their text parts
// concatenated under "text" ; binary parts are absent.
func outcomeToResult(o ToolOutcome) map[string]any {
	if o.Status == "" && len(o.Parts) == 0 && o.Error == "" {
		return nil
	}
	var text string
	for _, p := range o.Parts {
		text += p.Text
	}
	return map[string]any{
		"status": o.Status,
		"error":  o.Error,
		"text":   text,
	}
}

// applyResultMutation folds a transform_result mutation back into the
// outcome the agent will actually see. The hook mutated the projected
// result map (status / error / text) in place ; we re-project the
// runtime-visible fields. Multi-part outcomes collapse to the
// transformed text — the documented surface of transform_result is the
// textual result content.
func applyResultMutation(out *ToolOutcome, result map[string]any) {
	if out == nil || result == nil {
		return
	}
	if s, ok := result["status"].(string); ok && s != "" {
		out.Status = s
	}
	if e, ok := result["error"].(string); ok {
		out.Error = e
	}
	if txt, ok := result["text"].(string); ok {
		out.Parts = []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: txt},
		}
	}
}

// computeHookMetrics derives the data-driven hook-condition fields
// from the live turn state. These feed turn_count, message_count,
// tool_calls, context_pressure and content_contains — conditions that
// would otherwise NEVER fire because the engine handed them a
// zero-value payload. TurnCount is the count of user messages in the
// session (one user message = one user turn) ; UserMessage is the
// latest of them (for content_contains against the prompt).
func (e *Engine) computeHookMetrics(snap sessionstore.SessionSnapshot, agent *schema.Agent, llmContent string, toolCallsUsed int) hooks.Payload {
	var turns int
	var lastUser string
	for i := range snap.Messages {
		if snap.Messages[i].Role == "user" {
			turns++
			if snap.Messages[i].Content != "" {
				lastUser = snap.Messages[i].Content
			}
		}
	}
	// Context window state (occupancy + usable budget + pressure). A nil agent
	// has no resolvable window, so pressure stays neutral (MaxTokens 0).
	var cs contextsvc.Snapshot
	if agent != nil {
		cs = contextsvc.Resolve(snap, agent.Brain)
	} else {
		cs = contextsvc.Snapshot{TokensUsed: snap.ContextTokens}
	}
	used, maxTok := cs.TokensUsed, cs.MaxTokens
	// Prefer the FRESH context variable (the Tracker leads the durable
	// projection) so a context_pressure hook sees the real occupancy even
	// mid-turn — the same single source the per-round guard reads.
	if e != nil && agent != nil {
		if cv := e.freshContextView(snap.SessionID, snap, agent.Brain); cv.Limit > 0 {
			used, maxTok = cv.Used, cv.Limit
		}
	}
	openTasks, tasksSummary := openTaskState(snap.Todos)
	return hooks.Payload{
		MessageCount:  len(snap.Messages),
		TurnCount:     turns,
		ToolCallsUsed: toolCallsUsed,
		LLMContent:    llmContent,
		UserMessage:   lastUser,
		TokensUsed:    used,
		MaxTokens:     maxTok,
		OpenTasks:     openTasks,
		TasksSummary:  tasksSummary,
	}
}

// openTaskState summarises the unfinished task plan for the `stop` hook
// payload : count + a compact list ("t2 (in_progress), t3 (pending)"). A task
// is OPEN while it is pending or in_progress ; completed/done and blocked
// (acknowledged dead-end) are not. Feeds open_tasks + {{tasks.summary}}.
func openTaskState(todos []sessionstore.Todo) (int, string) {
	var open []string
	for _, t := range todos {
		switch t.Status {
		case "", "pending", "in_progress":
			status := t.Status
			if status == "" {
				status = "pending"
			}
			id := t.ID
			if id == "" {
				id = "?"
			}
			open = append(open, fmt.Sprintf("%s (%s)", id, status))
		}
	}
	return len(open), strings.Join(open, ", ")
}

// withTurnState copies the TurnState fields from m onto p, leaving p's
// event-specific fields (ToolName, ToolArgs, …) intact. Used to enrich
// every hook payload with the data-driven condition inputs.
func withTurnState(p, m hooks.Payload) hooks.Payload {
	p.MessageCount = m.MessageCount
	p.TurnCount = m.TurnCount
	p.ToolCallsUsed = m.ToolCallsUsed
	p.LLMContent = m.LLMContent
	p.UserMessage = m.UserMessage
	p.TokensUsed = m.TokensUsed
	p.MaxTokens = m.MaxTokens
	p.OpenTasks = m.OpenTasks
	p.TasksSummary = m.TasksSummary
	return p
}

// applyInjections persists every inject_message effect a fire produced,
// in order, so two hooks injecting on the same event (or a chain that
// injects several) all land. Best-effort per entry.
func (e *Engine) applyInjections(ctx context.Context, in TurnInput, tr *turn.Turn, injs []*hooks.MessageInjection) {
	for _, inj := range injs {
		e.applyInjection(ctx, in, tr, inj)
	}
}

// applyInjection persists an inject_message hook effect as a durable
// session message so the next LLM round (or next turn) sees it. The
// documented default role is "user" ; the message lands correlated to
// the firing turn. Best-effort : a failed append is logged, never
// fatal — a hook must not crash the turn.
func (e *Engine) applyInjection(ctx context.Context, in TurnInput, tr *turn.Turn, inj *hooks.MessageInjection) {
	if inj == nil || inj.Content == "" {
		return
	}
	role := inj.Role
	if role == "" {
		role = "user"
	}
	// System-role injects flow through the unified system-directive path
	// (durable EventSystemMessage, authority over the agent). User-role
	// injects stay synthetic user messages — a different intent.
	if role == "system" {
		e.injectSystemDirective(ctx, in, tr.ID, inj.Content, DirectiveHookInject, nil, nil)
		return
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventUserMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Message: &sessionstore.MessagePayload{
			Role:    role,
			Content: inj.Content,
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil {
		e.Logger.Warn("runtime: hook inject_message append failed",
			slog.String("err", err.Error()))
	}
}

// isCancellation reports whether err originates from ctx being
// cancelled (or the deadline expiring). We test both the wrapped
// error chain and ctx directly because some downstream layers
// (notably LLM clients) wrap context.Canceled in their own typed
// errors that don't always unwrap cleanly.
//
// RT-6 contract : an interrupted turn must emit EventTurnEnded with
// status="interrupted" — not "errored". This helper is the single
// place where the classification happens so audit / UI stay
// consistent regardless of which layer detected the cancellation.
func isCancellation(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			return true
		}
	}
	return false
}

// extractEmbeddedAuth pulls (apiKey, baseURL) out of the brain's
// declarative config. Three sources are inspected, in order :
//
//  1. brain.config.api_key (string) — the most common form.
//  2. brain.credential = "literal-key" — shorthand for api_key.
//  3. brain.credential.api_key (map) — legacy named-credential form.
//
// brain.config.base_url is forwarded when present (Ollama, vLLM,
// custom Anthropic-compatible endpoints). Returns ("", "") when no
// credential is embedded — the engine then defers to gateway mode.
func extractEmbeddedAuth(b schema.Brain) (apiKey, baseURL string) {
	if s, ok := b.Config["api_key"].(string); ok && s != "" {
		apiKey = s
	}
	if apiKey == "" {
		if s, ok := b.Credential.(string); ok && s != "" {
			apiKey = s
		} else if m, ok := b.Credential.(map[string]any); ok {
			if s, ok := m["api_key"].(string); ok && s != "" {
				apiKey = s
			}
		}
	}
	if s, ok := b.Config["base_url"].(string); ok && s != "" {
		baseURL = s
	}
	return apiKey, baseURL
}
