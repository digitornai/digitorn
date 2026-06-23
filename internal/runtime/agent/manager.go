// Package agent implements the multi-agent orchestrator : a per-root-session
// registry of delegated sub-agents, each running as its own goroutine.
//
// Design invariants (the reason this scales to a million nested agents) :
//
//   - Goroutine per agent, with a recover() guard : one agent panicking can
//     never crash another agent or the daemon.
//   - Nothing scarce is held while waiting. Spawn returns immediately ; Wait
//     blocks ONLY the calling goroutine on a done channel. The real bound is
//     the engine's per-call LLM semaphore, acquired around each LLM call and
//     never across a wait — so a parent waiting on a child holds no slot and
//     the child can always run. No agent ever blocks another.
//   - Lock-free hot path : the registry is sharded by root session
//     (sync.Map) ; per-agent telemetry is atomic counters. Status/List take a
//     brief per-root lock only.
//   - Cancellation tree : each agent's context derives from its parent's, so
//     cancelling an agent cancels its whole subtree.
//   - Guards : a max delegation depth and a per-root agent budget stop a
//     buggy agent from fork-bombing the machine.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// Defaults applied when a Manager field is zero.
const (
	DefaultMaxDepth         = 8
	DefaultMaxAgentsPerRoot = 100_000

	// DefaultAgentRetain bounds how long a terminal (completed/errored/cancelled)
	// agent stays in memory after it ended, so a coordinator can still Wait /
	// Status / List it within its turn. The durable trail lives in the sink, so
	// reaping the in-memory record loses nothing. agentReapInterval is how often
	// the sweeper runs.
	DefaultAgentRetain = 15 * time.Minute
	agentReapInterval  = 1 * time.Minute
)

var (
	ErrNotFound = errors.New("agent: not found")
	ErrDepth    = errors.New("agent: max delegation depth exceeded")
	ErrBudget   = errors.New("agent: per-root agent budget reached")
	ErrNoRunner = errors.New("agent: no runner attached")
)

// SubAgentRunner is the slice of the engine the manager needs : run a target
// agent as an isolated sub-turn. *runtime.Engine satisfies it via RunSubAgent.
type SubAgentRunner interface {
	RunSubAgent(ctx context.Context, spec runtime.SubAgentSpec) (runtime.AgentResult, error)
}

// EventSink publishes durable agent-lifecycle events (for client resync). nil
// = no durable events (the in-memory registry still works ; wired in P3).
type EventSink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

// SpawnRequest describes one delegation.
type SpawnRequest struct {
	AppID        string
	RootSession  string // the top-level session the whole tree lives under
	UserID       string
	UserJWT      string // gateway bearer forwarded to the sub-agent's turn (transient)
	AgentID      string // target logical agent id
	Task         string
	MemorySeed   string
	ParentRunID  string // "" = spawned by the entry agent (depth 0)
	ParentCallID string // tool call_id of the delegating `agent` call, for client chip binding
}

// Snapshot is a lock-free view of one agent, for status / list / resync.
type Snapshot struct {
	RunID         string `json:"run_id"`
	AgentID       string `json:"agent_id"`
	RootSession   string `json:"root_session"`
	ParentRunID   string `json:"parent_run_id,omitempty"`
	Status        string `json:"status"`
	Depth         int    `json:"depth"`
	StartedAtUnix int64  `json:"started_at"`
	DurationMs    int64  `json:"duration_ms"`
	ToolCalls     int64  `json:"tool_calls"`
	LLMCalls      int64  `json:"llm_calls"`
	TokensIn      int64  `json:"tokens_in"`
	TokensOut     int64  `json:"tokens_out"`
	Children      int64  `json:"children"`
	Content       string `json:"content,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Manager is the production multi-agent orchestrator.
type Manager struct {
	runner SubAgentRunner
	sink   EventSink
	logger *slog.Logger

	MaxDepth         int
	MaxAgentsPerRoot int

	// RetainCompleted bounds how long a terminal agent lingers after it ended
	// (0 → DefaultAgentRetain). now is injectable for tests.
	RetainCompleted time.Duration
	now             func() time.Time

	roots sync.Map // rootSession -> *rootTable

	reapStop chan struct{}
	reapOnce sync.Once
}

// New constructs a Manager. AttachRunner must be called before Spawn ; Start
// launches the background reaper.
func New(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{logger: logger, now: time.Now}
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) retain() time.Duration {
	if m.RetainCompleted > 0 {
		return m.RetainCompleted
	}
	return DefaultAgentRetain
}

func (m *Manager) AttachRunner(r SubAgentRunner) { m.runner = r }
func (m *Manager) AttachSink(s EventSink)        { m.sink = s }

func (m *Manager) maxDepth() int {
	if m.MaxDepth > 0 {
		return m.MaxDepth
	}
	return DefaultMaxDepth
}

func (m *Manager) maxAgents() int {
	if m.MaxAgentsPerRoot > 0 {
		return m.MaxAgentsPerRoot
	}
	return DefaultMaxAgentsPerRoot
}

type rootTable struct {
	mu     sync.Mutex
	agents map[string]*agentState
}

// agentState is the live state of one agent instance. Telemetry fields are
// atomic so the engine (via the Recorder) updates them lock-free in real time.
type agentState struct {
	runID        string
	agentID      string
	rootSession  string
	parentRunID  string
	parentCallID string
	depth        int
	startedNano  int64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	status    atomic.Value // string
	endedNano atomic.Int64
	result    atomic.Value // runtime.AgentResult
	errMsg    atomic.Value // string

	toolCalls   atomic.Int64
	llmCalls    atomic.Int64
	tokensIn    atomic.Int64
	tokensOut   atomic.Int64
	children    atomic.Int64
	currentTool atomic.Value // string — last tool name dispatched
}

// AddLLMCall / AddToolCall implement runtime.Recorder — the real-time, lock-free
// telemetry hook the engine calls around each LLM / tool call.
func (a *agentState) AddLLMCall(promptTokens, completionTokens int) {
	a.llmCalls.Add(1)
	a.tokensIn.Add(int64(promptTokens))
	a.tokensOut.Add(int64(completionTokens))
}

func (a *agentState) AddToolCall(toolName string) {
	a.toolCalls.Add(1)
	if toolName != "" {
		a.currentTool.Store(toolName)
	}
}

func (a *agentState) snapshot() Snapshot {
	s := Snapshot{
		RunID: a.runID, AgentID: a.agentID, RootSession: a.rootSession,
		ParentRunID: a.parentRunID, Depth: a.depth, StartedAtUnix: a.startedNano / int64(time.Second),
		ToolCalls: a.toolCalls.Load(), LLMCalls: a.llmCalls.Load(),
		TokensIn: a.tokensIn.Load(), TokensOut: a.tokensOut.Load(), Children: a.children.Load(),
	}
	if v, ok := a.status.Load().(string); ok {
		s.Status = v
	}
	if v, ok := a.errMsg.Load().(string); ok {
		s.Error = v
	}
	if v, ok := a.result.Load().(runtime.AgentResult); ok {
		s.Content = v.Content
	}
	end := a.endedNano.Load()
	if end == 0 {
		s.DurationMs = (time.Now().UnixNano() - a.startedNano) / int64(time.Millisecond)
	} else {
		s.DurationMs = (end - a.startedNano) / int64(time.Millisecond)
	}
	return s
}

func (m *Manager) rootFor(root string) *rootTable {
	if v, ok := m.roots.Load(root); ok {
		return v.(*rootTable)
	}
	fresh := &rootTable{agents: map[string]*agentState{}}
	if actual, loaded := m.roots.LoadOrStore(root, fresh); loaded {
		return actual.(*rootTable)
	}
	return fresh
}

// lockRoot returns the root table LOCKED, guaranteed to still be the one
// registered in m.roots. The reaper deletes an empty table while holding its
// lock ; without this validation Spawn could add an agent to a table the reaper
// is removing, orphaning the agent (and its goroutine) outside m.roots. The loop
// re-fetches if our table was reaped between rootFor and the lock — and because
// the reaper must HOLD the lock to delete, a validated holder keeps the table
// live for its whole critical section. Caller unlocks rt.mu.
func (m *Manager) lockRoot(root string) *rootTable {
	for {
		rt := m.rootFor(root)
		rt.mu.Lock()
		if cur, ok := m.roots.Load(root); ok && cur.(*rootTable) == rt {
			return rt
		}
		rt.mu.Unlock()
	}
}

// terminal reports whether the agent has finished (its goroutine has recorded a
// final status and endedNano). Running agents are never reaped.
func (a *agentState) terminal() bool {
	if a.endedNano.Load() == 0 {
		return false
	}
	switch a.status.Load().(string) {
	case "completed", "errored", "cancelled":
		return true
	}
	return false
}

// Start launches the background reaper that drops terminal agents (and empty
// root tables) once they're older than RetainCompleted, bounding memory under a
// long-lived daemon with heavy delegation. Idempotent ; Stop ends it. The reaper
// also exits if ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	m.reapStop = make(chan struct{})
	stop := m.reapStop
	go func() {
		defer func() { _ = recover() }()
		t := time.NewTicker(agentReapInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.reapAll()
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop ends the background reaper. Safe to call once ; a no-op if never started.
func (m *Manager) Stop() {
	if m.reapStop == nil {
		return
	}
	m.reapOnce.Do(func() { close(m.reapStop) })
}

// reapAll sweeps every root, deleting terminal agents past the retention window
// and removing root tables left empty. Deleting an empty table from m.roots is
// done UNDER its lock so it can't race a concurrent Spawn (see lockRoot).
func (m *Manager) reapAll() {
	cutoff := m.clock().Add(-m.retain()).UnixNano()
	m.roots.Range(func(k, v any) bool {
		rt := v.(*rootTable)
		rt.mu.Lock()
		for id, a := range rt.agents {
			if a.terminal() && a.endedNano.Load() < cutoff {
				delete(rt.agents, id)
			}
		}
		if len(rt.agents) == 0 {
			m.roots.Delete(k)
		}
		rt.mu.Unlock()
		return true
	})
}

// Spawn launches a sub-agent and returns its distinct run id IMMEDIATELY. The
// agent runs in its own goroutine ; the caller does not block. Enforces the
// depth + budget guards.
func (m *Manager) Spawn(_ context.Context, req SpawnRequest) (string, error) {
	if m.runner == nil {
		return "", ErrNoRunner
	}
	rt := m.lockRoot(req.RootSession)
	depth := 0
	var parentCtx context.Context = context.Background()
	if req.ParentRunID != "" {
		if p := rt.agents[req.ParentRunID]; p != nil {
			depth = p.depth + 1
			parentCtx = p.ctx
		}
	}
	if depth > m.maxDepth() {
		rt.mu.Unlock()
		return "", fmt.Errorf("%w (%d)", ErrDepth, m.maxDepth())
	}
	if len(rt.agents) >= m.maxAgents() {
		rt.mu.Unlock()
		return "", fmt.Errorf("%w (%d)", ErrBudget, m.maxAgents())
	}

	runID := runtime.NewAgentRunID(req.AgentID)
	actx, cancel := context.WithCancel(parentCtx)
	a := &agentState{
		runID: runID, agentID: req.AgentID, rootSession: req.RootSession,
		parentRunID: req.ParentRunID, parentCallID: req.ParentCallID,
		depth: depth, startedNano: m.clock().UnixNano(),
		ctx: actx, cancel: cancel, done: make(chan struct{}),
	}
	a.status.Store("running")
	rt.agents[runID] = a
	if req.ParentRunID != "" {
		if p := rt.agents[req.ParentRunID]; p != nil {
			p.children.Add(1)
		}
	}
	rt.mu.Unlock()

	m.emit(a, "running")
	go m.runAgent(a, req)
	return runID, nil
}

// runAgent is the per-agent goroutine. A panic here is contained : it marks the
// agent errored and never propagates.
func (m *Manager) runAgent(a *agentState, req SpawnRequest) {
	// Always release the agent's context when its goroutine exits. Without
	// this, a child agent stays attached to its parent's cancellation
	// propagation list for the lifetime of the parent — a steady leak under
	// repeated delegation.
	defer a.cancel()
	defer close(a.done)
	defer func() {
		if r := recover(); r != nil {
			a.endedNano.Store(m.clock().UnixNano())
			a.errMsg.Store(fmt.Sprintf("panic: %v", r))
			a.status.Store("errored")
			m.emit(a, "errored")
			m.logger.Error("agent_panic", slog.String("run_id", a.runID), slog.Any("panic", r))
		}
	}()

	// Periodic progress ticker: emit durable telemetry every 5 s so reconnecting
	// clients always see an up-to-date state, even for long-running agents.
	tickStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.emitProgress(a, "")
			case <-tickStop:
				return
			case <-a.ctx.Done():
				return
			}
		}
	}()

	// Attach this agent as the telemetry recorder so the engine updates its
	// counters in real time while the sub-turn runs.
	ctx := runtime.WithRecorder(a.ctx, a)
	res, err := m.runner.RunSubAgent(ctx, runtime.SubAgentSpec{
		AppID: req.AppID, ParentSession: req.RootSession, UserID: req.UserID, UserJWT: req.UserJWT,
		AgentID: req.AgentID, RunID: a.runID, Task: req.Task, MemorySeed: req.MemorySeed, Depth: a.depth,
	})

	close(tickStop) // stop the telemetry ticker
	a.endedNano.Store(m.clock().UnixNano())
	a.result.Store(res)
	status := "completed"
	if err != nil {
		status = "errored"
		a.errMsg.Store(err.Error())
		if a.ctx.Err() != nil {
			status = "cancelled"
			a.errMsg.Store(a.ctx.Err().Error())
		}
	}
	a.status.Store(status)
	m.emit(a, status)
}

func (m *Manager) lookup(root, runID string) *agentState {
	v, ok := m.roots.Load(root)
	if !ok {
		return nil
	}
	rt := v.(*rootTable)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.agents[runID]
}

// Wait blocks until the agent finishes or the timeout (0 = none) expires. It
// blocks ONLY the calling goroutine — other agents run undisturbed.
func (m *Manager) Wait(ctx context.Context, root, runID string, timeout time.Duration) (Snapshot, error) {
	a := m.lookup(root, runID)
	if a == nil {
		return Snapshot{}, ErrNotFound
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case <-a.done:
		return a.snapshot(), nil
	case <-ctx.Done():
		return a.snapshot(), ctx.Err()
	}
}

// WaitAll waits for several agents concurrently and returns their snapshots in
// input order. All agents are awaited simultaneously — the first to finish
// unblocks its slot immediately rather than waiting for earlier agents to
// complete first. A timeout applies to the whole batch.
func (m *Manager) WaitAll(ctx context.Context, root string, runIDs []string, timeout time.Duration) ([]Snapshot, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	type result struct {
		idx      int
		snapshot Snapshot
		err      error
	}

	// Buffered to the number of agents so every goroutine can deliver without
	// blocking even if the collector stopped reading early (cancellation).
	resCh := make(chan result, len(runIDs))

	for i, id := range runIDs {
		go func(idx int, runID string) {
			s, err := m.Wait(ctx, root, runID, 0)
			resCh <- result{idx: idx, snapshot: s, err: err}
		}(i, id)
	}

	out := make([]Snapshot, len(runIDs))
	var firstErr error
	for range runIDs {
		r := <-resCh
		out[r.idx] = r.snapshot
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	return out, firstErr
}

// Status returns one agent's live snapshot.
func (m *Manager) Status(root, runID string) (Snapshot, error) {
	a := m.lookup(root, runID)
	if a == nil {
		return Snapshot{}, ErrNotFound
	}
	return a.snapshot(), nil
}

// List returns the whole agent tree for a root session.
func (m *Manager) List(root string) []Snapshot {
	v, ok := m.roots.Load(root)
	if !ok {
		return nil
	}
	rt := v.(*rootTable)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]Snapshot, 0, len(rt.agents))
	for _, a := range rt.agents {
		out = append(out, a.snapshot())
	}
	return out
}

// Cancel stops an agent and its whole subtree (ctx-tree cancellation). Returns
// immediately ; the agent's goroutine observes the cancel and unwinds.
func (m *Manager) Cancel(root, runID string) error {
	a := m.lookup(root, runID)
	if a == nil {
		return ErrNotFound
	}
	a.cancel()
	return nil
}

// CancelAll stops EVERY agent under a root session — the whole delegated tree.
// A user "stop" (session abort) must halt all delegated work, not just the
// coordinator's turn : sub-agents run on independent contexts (so they never
// block the parent), so the turn's own ctx cancel can't reach them. Each
// agent's goroutine observes its cancel and unwinds to "cancelled". Returns the
// number of agents signalled. The cancels are gathered under the lock and
// fired outside it so a cancel callback can never deadlock against Spawn/Status.
func (m *Manager) CancelAll(root string) int {
	v, ok := m.roots.Load(root)
	if !ok {
		return 0
	}
	rt := v.(*rootTable)
	rt.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(rt.agents))
	for _, a := range rt.agents {
		cancels = append(cancels, a.cancel)
	}
	rt.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// emit publishes a DURABLE, seq'd agent-lifecycle event into the root session
// when a sink is wired. These project into SessionSnapshot.Children and replay
// through the existing "since seq" path, so a reconnecting client reconstructs
// the agent tree with no gaps. "running" → EventAgentSpawn ; any terminal
// status → EventAgentResult. Best-effort : a sink error never blocks the agent.
// emitProgress publishes a durable agent_progress snapshot so every client —
// including those that reconnect after a network hiccup or a daemon restart —
// can reconstruct the live agent tree from the event log without hitting the
// in-memory registry. Called on every tool/LLM completion and every 5 s tick.
func (m *Manager) emitProgress(a *agentState, currentTool string) {
	if m.sink == nil {
		return
	}
	durationMs := int64(0)
	if end := a.endedNano.Load(); end > 0 {
		durationMs = (end - a.startedNano) / int64(time.Millisecond)
	} else {
		durationMs = (time.Now().UnixNano() - a.startedNano) / int64(time.Millisecond)
	}
	status := "running"
	if v, ok := a.status.Load().(string); ok && v != "" {
		status = v
	}
	_, _ = m.sink.AppendDurable(context.Background(), sessionstore.Event{
		Type:          sessionstore.EventAgentProgress,
		SessionID:     a.rootSession,
		CorrelationID: a.runID,
		Agent: &sessionstore.AgentPayload{
			RunID:       a.runID,
			ParentRunID: a.parentRunID,
			Kind:        a.agentID,
			Status:      status,
			Depth:       a.depth,
			ToolCalls:   a.toolCalls.Load(),
			LLMCalls:    a.llmCalls.Load(),
			TokensIn:    a.tokensIn.Load(),
			TokensOut:   a.tokensOut.Load(),
			Children:    a.children.Load(),
			DurationMs:  durationMs,
			CurrentTool: func() string {
				if currentTool != "" {
					return currentTool
				}
				v, _ := a.currentTool.Load().(string)
				return v
			}(),
		},
	})
}

func (m *Manager) emit(a *agentState, status string) {
	if m.sink == nil {
		m.logger.Warn("agent_emit_skipped", slog.String("reason", "no sink attached"),
			slog.String("run_id", a.runID), slog.String("status", status))
		return
	}
	payload := &sessionstore.AgentPayload{
		RunID:          a.runID,
		ParentRunID:    a.parentRunID,
		ParentCallID:   a.parentCallID,
		Kind:           a.agentID,
		ChildSessionID: a.rootSession + "::agent::" + a.runID,
		Status:         status,
		Depth:          a.depth,
	}
	evType := sessionstore.EventAgentSpawn
	if status != "running" {
		evType = sessionstore.EventAgentResult
		if v, ok := a.result.Load().(runtime.AgentResult); ok {
			payload.ResultSummary = truncate(v.Content, 500)
		}
		// Terminal telemetry, durable : the live registry can be gone after a
		// daemon restart, so persist the final counts with the result so the
		// tree reconstructs fully from disk.
		payload.ToolCalls = a.toolCalls.Load()
		payload.LLMCalls = a.llmCalls.Load()
		payload.TokensIn = a.tokensIn.Load()
		payload.TokensOut = a.tokensOut.Load()
		if end := a.endedNano.Load(); end > 0 {
			payload.DurationMs = (end - a.startedNano) / int64(time.Millisecond)
		}
	}
	if _, err := m.sink.AppendDurable(context.Background(), sessionstore.Event{
		Type:          evType,
		SessionID:     a.rootSession,
		CorrelationID: a.runID,
		Agent:         payload,
	}); err != nil {
		// Never silently lose a lifecycle event : a dropped agent_spawn /
		// agent_result leaves clients blind to the sub-agent. Surface it.
		m.logger.Warn("agent_emit_failed", slog.String("type", string(evType)),
			slog.String("session", a.rootSession), slog.String("run_id", a.runID),
			slog.Any("err", err))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
