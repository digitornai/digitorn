package server

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/contextcompact"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/safego"
)

// contextBGSummaryEnabled is the CTX-8 kill-switch. Default OFF — the compaction
// path is exactly the legacy (inline-summarize) behaviour until this is set, so
// enabling the background, non-blocking summary in prod is a one-line env flip
// (DIGITORN_CONTEXT_BG_SUMMARY=1) + restart, with an instant rollback.
func contextBGSummaryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_CONTEXT_BG_SUMMARY"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// summaryMaintainer keeps a high-fidelity LLM summary of each session's aged
// region PREPARED ahead of the compaction gate, entirely OFF the turn loop
// (CTX-8). The runtime calls Touch (non-blocking) with the turn's user JWT; a
// bounded pool coalesces per session, runs the (possibly slow) summary LLM call
// in the background, and persists an EventContextSummaryPrepared candidate the
// gate applies INSTANTLY. Nothing the turn loop does blocks on this: Touch never
// blocks, the LLM call happens only here, and a failure simply leaves no
// prepared candidate (the gate truncates instantly instead).
type summaryMaintainer struct {
	store     *sessionstore.Bus
	compactor *contextCompactor
	llm       chatLLM
	logger    *slog.Logger
	timeout   time.Duration

	mu      sync.Mutex
	pending map[string]struct{}
	jwts    map[string]string // freshest user JWT per session (gateway-mode auth); in-memory only
	queue   chan string
	stop    chan struct{}
	wg      sync.WaitGroup
	started bool
}

func newSummaryMaintainer(store *sessionstore.Bus, compactor *contextCompactor, client chatLLM, logger *slog.Logger) *summaryMaintainer {
	return &summaryMaintainer{
		store:     store,
		compactor: compactor,
		llm:       client,
		logger:    logger,
		timeout:   5 * time.Minute, // generous: it is off the loop — quality over speed
		pending:   make(map[string]struct{}),
		jwts:      make(map[string]string),
		queue:     make(chan string, 4096),
		stop:      make(chan struct{}),
	}
}

func (m *summaryMaintainer) Start(workers int) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		m.wg.Add(1)
		safego.Go("server.summaryMaintainer", func() {
			defer m.wg.Done()
			m.loop()
		})
	}
}

func (m *summaryMaintainer) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	close(m.stop)
	m.mu.Unlock()
	m.wg.Wait()
}

// Touch signals a session whose aged region may need a fresh prepared summary.
// jwt (the turn's user bearer) is stashed so the off-loop LLM call authenticates
// in gateway mode. NEVER blocks: a duplicate pending touch is coalesced and a
// saturated queue drops the marker (the next Touch re-enqueues).
func (m *summaryMaintainer) Touch(sessionID, jwt string) {
	if m == nil || sessionID == "" {
		return
	}
	m.mu.Lock()
	if jwt != "" {
		m.jwts[sessionID] = jwt
	}
	if _, dup := m.pending[sessionID]; dup {
		m.mu.Unlock()
		return
	}
	m.pending[sessionID] = struct{}{}
	m.mu.Unlock()

	select {
	case m.queue <- sessionID:
	default:
		m.mu.Lock()
		delete(m.pending, sessionID)
		m.mu.Unlock()
	}
}

func (m *summaryMaintainer) loop() {
	for {
		select {
		case <-m.stop:
			return
		case sid := <-m.queue:
			m.mu.Lock()
			delete(m.pending, sid)
			m.mu.Unlock()
			m.prepare(sid)
		}
	}
}

func (m *summaryMaintainer) takeJWT(sid string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jwts[sid]
}

// prepare computes (off the loop) a high-fidelity LLM summary of the aged region
// and persists it as a candidate. Recover-guarded so a panic can never crash the
// pool. Best-effort: any early return / LLM failure simply leaves no candidate.
func (m *summaryMaintainer) prepare(sid string) {
	defer safego.Recover("server.summaryMaintainer.prepare")
	if m.store == nil || m.compactor == nil {
		return
	}
	state, err := m.store.State(sid)
	if err != nil || state == nil {
		return
	}
	snap := state.Snapshot()
	if len(snap.Messages) == 0 {
		return
	}

	cfg, brain, byok := m.compactor.resolveContextConfig(context.Background(), snap.AppID)
	keep := 0
	if cfg != nil {
		keep = cfg.KeepRecent
	}
	keepRecent := contextcompact.KeepRecentOrDefault(keep)

	// Match the summarize gate's cut (count-based) so the prepared CoversSeq lines
	// up with the region the gate would drop.
	cut := contextcompact.SafeSplitIndex(snap.Messages, keepRecent)
	if cut == 0 {
		return
	}
	coversSeq := snap.Messages[cut-1].Seq
	if coversSeq == 0 {
		return
	}
	// Hysteresis: do nothing if we already prepared (or already applied) a summary
	// covering at least this far — avoids re-summarising the same region on churn.
	if snap.PreparedSummary != nil && coversSeq <= snap.PreparedSummary.CoversSeq {
		return
	}
	if snap.ContextCompaction != nil && coversSeq <= snap.ContextCompaction.CutoffSeq {
		return
	}

	summaryMax := 2048
	if cfg != nil && cfg.SummaryMaxTokens > 0 {
		summaryMax = cfg.SummaryMaxTokens
	}
	prior := ""
	if snap.ContextCompaction != nil {
		prior = snap.ContextCompaction.Summary
	}
	s := &llmSummarizer{client: m.llm, brain: brain, sessionID: sid, byok: byok, userJWT: m.takeJWT(sid), logger: m.logger}

	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	res := contextcompact.Summarize(ctx, snap.Messages, keepRecent, s, summaryMax, snap.Goal, prior)
	// Only a REAL LLM summary becomes a prepared candidate. A truncate fallback
	// (LLM failed / empty / no brain) prepares nothing — the gate truncates
	// instantly instead, and a later turn retries the summary.
	if res.Strategy != contextcompact.StrategySummarize || res.CutoffSeq == 0 || strings.TrimSpace(res.Summary) == "" {
		return
	}
	if _, err := m.store.Append(ctx, sessionstore.Event{
		Type:      sessionstore.EventContextSummaryPrepared,
		SessionID: sid,
		AppID:     snap.AppID,
		UserID:    snap.UserID,
		CtxSummary: &sessionstore.ContextSummaryPayload{
			Summary:   res.Summary,
			CoversSeq: res.CutoffSeq,
			Model:     brain.Model,
		},
	}); err != nil {
		return
	}
	if m.logger != nil {
		m.logger.Info("context summary prepared (background)",
			slog.String("session_id", sid),
			slog.Uint64("covers_seq", res.CutoffSeq))
	}
}
