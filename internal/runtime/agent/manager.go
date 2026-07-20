package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const (
	DefaultMaxDepth         = 20
	DefaultMaxAgentsPerRoot = 100_000

	DefaultAgentRetain = 30 * time.Minute
	agentReapInterval  = 1 * time.Minute
)

var (
	ErrNotFound = errors.New("agent: not found")
	ErrDepth    = errors.New("agent: max delegation depth exceeded")
	ErrBudget   = errors.New("agent: per-root agent budget reached")
	ErrNoRunner = errors.New("agent: no runner attached")
)

type SubAgentRunner interface {
	RunSubAgent(ctx context.Context, spec runtime.SubAgentSpec) (runtime.AgentResult, error)
}

type EventSink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

type SpawnRequest struct {
	AppID        string
	RootSession  string
	UserID       string
	UserJWT      string
	AgentID      string
	Task         string
	MemorySeed   string
	ParentRunID  string
	ParentCallID string

	// InheritContext = fork mode (seed with the parent transcript). Default
	// false = current isolated sub-agent behavior.
	InheritContext bool
}

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

type Manager struct {
	runner SubAgentRunner
	sink   EventSink
	logger *slog.Logger

	MaxDepth         int
	MaxAgentsPerRoot int

	RetainCompleted time.Duration
	now             func() time.Time

	roots sync.Map

	reapStop chan struct{}
	reapOnce sync.Once
}

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

type agentState struct {
	runID        string
	agentID      string
	rootSession  string
	parentRunID  string
	parentCallID string
	depth        int
	fork         bool
	startedNano  int64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	status       atomic.Value
	endedNano    atomic.Int64
	result       atomic.Value
	errMsg       atomic.Value
	cancelReason atomic.Value

	toolCalls   atomic.Int64
	llmCalls    atomic.Int64
	tokensIn    atomic.Int64
	tokensOut   atomic.Int64
	children    atomic.Int64
	currentTool atomic.Value
}

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

func (m *Manager) Stop() {
	if m.reapStop == nil {
		return
	}
	m.reapOnce.Do(func() { close(m.reapStop) })
}

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

func (m *Manager) SpawnBatch(_ context.Context, reqs []SpawnRequest) ([]string, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if m.runner == nil {
		return nil, ErrNoRunner
	}
	root := reqs[0].RootSession
	rt := m.lockRoot(root)

	if len(rt.agents)+len(reqs) > m.maxAgents() {
		rt.mu.Unlock()
		return nil, fmt.Errorf("%w (%d)", ErrBudget, m.maxAgents())
	}

	runIDs := make([]string, len(reqs))
	states := make([]*agentState, len(reqs))

	for i, req := range reqs {
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
			return nil, fmt.Errorf("%w (%d)", ErrDepth, m.maxDepth())
		}
		runID := runtime.NewAgentRunID(req.AgentID)
		actx, cancel := context.WithCancel(parentCtx)
		a := &agentState{
			runID: runID, agentID: req.AgentID, rootSession: req.RootSession,
			parentRunID: req.ParentRunID, parentCallID: req.ParentCallID,
			depth: depth, fork: req.InheritContext, startedNano: m.clock().UnixNano(),
			ctx: actx, cancel: cancel, done: make(chan struct{}),
		}
		a.status.Store("running")
		rt.agents[runID] = a
		if req.ParentRunID != "" {
			if p := rt.agents[req.ParentRunID]; p != nil {
				p.children.Add(1)
			}
		}
		runIDs[i] = runID
		states[i] = a
	}
	rt.mu.Unlock()

	for i, a := range states {
		m.emit(a, "running")
		go m.runAgent(a, reqs[i])
	}
	return runIDs, nil
}

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
		depth: depth, fork: req.InheritContext, startedNano: m.clock().UnixNano(),
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

func (m *Manager) runAgent(a *agentState, req SpawnRequest) {

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

	ctx := runtime.WithRecorder(a.ctx, a)
	res, err := m.runner.RunSubAgent(ctx, runtime.SubAgentSpec{
		AppID: req.AppID, ParentSession: req.RootSession, UserID: req.UserID, UserJWT: req.UserJWT,
		AgentID: req.AgentID, RunID: a.runID, Task: req.Task, MemorySeed: req.MemorySeed, Depth: a.depth,
		InheritContext: req.InheritContext,
	})

	close(tickStop)
	a.endedNano.Store(m.clock().UnixNano())
	a.result.Store(res)
	status := "completed"
	if err != nil {
		status = "errored"
		a.errMsg.Store(err.Error())
		if a.ctx.Err() != nil {
			status = "cancelled"
			if reason, ok := a.cancelReason.Load().(string); ok && reason != "" {
				a.errMsg.Store(reason)
			} else {
				a.errMsg.Store(a.ctx.Err().Error())
			}
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

func (m *Manager) Status(root, runID string) (Snapshot, error) {
	a := m.lookup(root, runID)
	if a == nil {
		return Snapshot{}, ErrNotFound
	}
	return a.snapshot(), nil
}

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

func (m *Manager) Cancel(root, runID string) error {
	a := m.lookup(root, runID)
	if a == nil {
		return ErrNotFound
	}
	a.cancel()
	return nil
}

func (m *Manager) CancelWithReason(root, runID, reason string) (int, error) {
	a := m.lookup(root, runID)
	if a == nil {
		return 0, ErrNotFound
	}
	if reason != "" {
		a.cancelReason.Store(reason)
	}
	return m.CancelTree(root, runID), nil
}

func (m *Manager) CancelTree(root, runID string) int {
	v, ok := m.roots.Load(root)
	if !ok {
		return 0
	}
	rt := v.(*rootTable)
	rt.mu.Lock()
	var cancels []context.CancelFunc
	for _, a := range rt.agents {
		if a.runID == runID || a.parentRunID == runID || isDescendant(rt.agents, a.runID, runID) {
			cancels = append(cancels, a.cancel)
		}
	}
	rt.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

func isDescendant(agents map[string]*agentState, runID, ancestorID string) bool {
	a, ok := agents[runID]
	if !ok {
		return false
	}
	if a.parentRunID == ancestorID {
		return true
	}
	if a.parentRunID == "" {
		return false
	}
	return isDescendant(agents, a.parentRunID, ancestorID)
}

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
			Fork:        a.fork,
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
		Fork:           a.fork,
	}
	evType := sessionstore.EventAgentSpawn
	if status != "running" {
		evType = sessionstore.EventAgentResult
		if v, ok := a.result.Load().(runtime.AgentResult); ok {
			payload.ResultSummary = truncate(v.Content, 500)
		}

		payload.ToolCalls = a.toolCalls.Load()
		payload.LLMCalls = a.llmCalls.Load()
		payload.TokensIn = a.tokensIn.Load()
		payload.TokensOut = a.tokensOut.Load()
		if end := a.endedNano.Load(); end > 0 {
			payload.DurationMs = (end - a.startedNano) / int64(time.Millisecond)
		}
	}
	ev := sessionstore.Event{
		Type:          evType,
		SessionID:     a.rootSession,
		CorrelationID: a.runID,
		Agent:         payload,
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := m.sink.AppendDurable(context.Background(), ev); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	m.logger.Warn("agent_emit_failed", slog.String("type", string(evType)),
		slog.String("session", a.rootSession), slog.String("run_id", a.runID),
		slog.Any("err", lastErr))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
