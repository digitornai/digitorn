package server

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/contextcompact"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/safego"
)

// contextBGSummaryDisabled is the kill-switch. Default ON — set
// DIGITORN_CONTEXT_BG_SUMMARY=0 to fall back to inline summarize.
func contextBGSummaryDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_CONTEXT_BG_SUMMARY"))) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// summaryMaintainer keeps a high-fidelity LLM summary of each session's aged
// region PREPARED ahead of the compaction gate, entirely off the turn loop.
// The runtime calls Touch (non-blocking) after each turn; a bounded worker
// pool coalesces per-session, runs the (possibly slow) summary LLM call in the
// background, and persists an EventContextSummaryPrepared candidate the gate
// applies instantly. When the prepared candidate already covers the region the
// gate would drop, compaction is zero-latency: no LLM call on the turn loop.
//
// Delta mode (default): only new messages since the last prepared coverage are
// sent to the summary LLM — 5-10x cheaper than re-summarising the full history.
// The prior summary provides the accumulated context; the LLM merges the delta
// into it, producing a rolling, cumulative handoff.
type summaryMaintainer struct {
	store     *sessionstore.Bus
	compactor *contextCompactor
	llm       chatLLM
	logger    *slog.Logger
	timeout   time.Duration

	mu      sync.Mutex
	pending map[string]struct{}
	jwts    map[string]string // freshest user JWT per session; in-memory only
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
		timeout:   5 * time.Minute,
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
		workers = 8
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
// jwt is stashed so the off-loop LLM call authenticates in gateway mode.
// NEVER blocks: a duplicate pending touch is coalesced and a saturated queue
// drops the marker (the next Touch re-enqueues).
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

// prepare computes (off the loop) a high-fidelity LLM summary and persists it
// as a candidate. Uses delta-mode when an existing summary already covers part
// of the history — only new messages are sent to the LLM, dramatically reducing
// cost for long sessions. Recover-guarded: panics never crash the pool.
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

	cfg, brain, byok := m.compactor.resolveContextConfig(context.Background(), snap.AppID, sid)
	keep := 0
	if cfg != nil {
		keep = cfg.KeepRecent
	}
	keepRecent := contextcompact.KeepRecentOrDefault(keep)

	// Protect the last user message (same guard as compactor).
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if snap.Messages[i].Role == "user" {
			if minKeep := len(snap.Messages) - i; keepRecent < minKeep {
				keepRecent = minKeep
			}
			break
		}
	}

	cut := contextcompact.SafeSplitIndex(snap.Messages, keepRecent)
	if cut == 0 {
		return
	}
	coversSeq := snap.Messages[cut-1].Seq
	if coversSeq == 0 {
		return
	}

	// Hysteresis: already have a summary that covers this far or further.
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

	// Resolve best available prior: PreparedSummary wins over ContextCompaction
	// when it covers further (always the case if maintained correctly).
	existingCoverage := uint64(0)
	priorSummary := ""
	if snap.ContextCompaction != nil && snap.ContextCompaction.CutoffSeq > 0 {
		existingCoverage = snap.ContextCompaction.CutoffSeq
		priorSummary = snap.ContextCompaction.Summary
	}
	if snap.PreparedSummary != nil && snap.PreparedSummary.CoversSeq > existingCoverage {
		existingCoverage = snap.PreparedSummary.CoversSeq
		priorSummary = snap.PreparedSummary.Summary
	}

	s := &llmSummarizer{
		client:    m.llm,
		brain:     brain,
		sessionID: sid,
		byok:      byok,
		userJWT:   m.takeJWT(sid),
		logger:    m.logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	var res contextcompact.Result

	if existingCoverage > 0 {
		// DELTA MODE: only summarize messages since existing coverage.
		// We build a synthetic slice [delta + tail] so SafeSplitIndex inside
		// Summarize computes cut = len(delta), summarizing only the new portion.
		// The prior summary provides cumulative context to the LLM.
		var delta []sessionstore.Message
		for i := range snap.Messages[:cut] {
			if snap.Messages[i].Seq > existingCoverage {
				delta = append(delta, snap.Messages[i])
			}
		}
		if len(delta) == 0 {
			return // existing coverage is already up to date
		}
		// tail = snap.Messages[cut:] has exactly keepRecent messages.
		// delta + tail → SafeSplitIndex gives cut = len(delta), so Summarize
		// drops only the delta and keeps the tail verbatim. Perfect.
		synthetic := append(delta, snap.Messages[cut:]...)
		res = contextcompact.Summarize(ctx, synthetic, keepRecent, s, summaryMax, snap.Goal, priorSummary)
		// CutoffSeq from Summarize points to delta's last message = snap.Messages[cut-1].Seq ✓
	} else {
		// FULL MODE: first summarization, no prior coverage.
		res = contextcompact.Summarize(ctx, snap.Messages, keepRecent, s, summaryMax, snap.Goal, "")
	}

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
			slog.Uint64("covers_seq", res.CutoffSeq),
			slog.Bool("delta_mode", existingCoverage > 0))
	}
}
