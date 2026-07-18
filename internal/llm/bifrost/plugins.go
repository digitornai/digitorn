package bifrost

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// Three lightweight plugins for the digitorn LLM worker. None of them
// duplicates work the external gateway already does (rate-limit, quota,
// cost accounting) — they exist only for *local* observability and
// fail-fast on dead providers.
//
// Phase-2 hot-path contract (all three plugins):
//   - PreLLMHook  must take ZERO mutexes on the success path.
//   - PostLLMHook is allowed one sync.Map.LoadOrStore on cold path,
//     all subsequent calls are atomic.
//   - All shared state is keyed by provider via sync.Map ; per-provider
//     state uses atomic ints + a fixed-size ring buffer of failure
//     timestamps. No appending slice, no GC pressure under load.
//
// This eliminates the global mutex that previously serialised every
// chat call. Verified by `plugins_concurrent_test.go` with go -race.

// ---------- 1. Audit minimal ----------

const auditStartCtxKey ctxKey = 100

// AuditPlugin emits one structured log line per LLM call with provider,
// model, latency, success flag. Optional ; disable via SetEnabled(false).
type AuditPlugin struct {
	logger  *slog.Logger
	enabled atomic.Bool
}

func NewAuditPlugin(logger *slog.Logger, enabled bool) *AuditPlugin {
	if logger == nil {
		logger = slog.Default()
	}
	p := &AuditPlugin{logger: logger}
	p.enabled.Store(enabled)
	return p
}

func (p *AuditPlugin) GetName() string   { return "digitorn.audit" }
func (p *AuditPlugin) Cleanup() error    { return nil }
func (p *AuditPlugin) SetEnabled(b bool) { p.enabled.Store(b) }

func (p *AuditPlugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if !p.enabled.Load() {
		return req, nil, nil
	}
	ctx.SetValue(auditStartCtxKey, time.Now())
	return req, nil, nil
}

func (p *AuditPlugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if !p.enabled.Load() {
		return resp, bifrostErr, nil
	}
	v := ctx.Value(auditStartCtxKey)
	start, _ := v.(time.Time)
	var latencyMs int64
	if !start.IsZero() {
		latencyMs = time.Since(start).Milliseconds()
	}
	success := bifrostErr == nil
	provider := ""
	model := ""
	if resp != nil && resp.ChatResponse != nil {
		model = resp.ChatResponse.Model
		_ = provider // provider not exposed on response; left blank
	}
	attrs := []any{
		slog.String("event", "llm.call"),
		slog.Int64("latency_ms", latencyMs),
		slog.Bool("success", success),
		slog.String("model", model),
	}
	if !success && bifrostErr.Error != nil {
		attrs = append(attrs, slog.String("err", bifrostErr.Error.Message))
	}
	p.logger.Info("llm.call", attrs...)
	return resp, bifrostErr, nil
}

// ---------- 2. Metrics counters ----------

// MetricsPlugin maintains in-memory counters + a rolling latency
// histogram per provider. Exposed via Stats() ; no Prometheus dep — the
// caller can scrape Stats() into its own metrics pipeline.
//
// Phase-2 change: per-provider lookup is sync.Map (lock-free after
// warmup) instead of sync.RWMutex + map. After the first request for
// a given provider, every subsequent call is one atomic.Load.
type MetricsPlugin struct {
	// perProvider: sync.Map[string]*providerMetrics. Lock-free reads,
	// single Store on the cold-path (LoadOrStore at first sight of
	// a provider name). Replaces the previous sync.RWMutex+map which
	// took an RLock on every chat call.
	perProvider sync.Map

	totalRequests  atomic.Uint64
	totalErrors    atomic.Uint64
	totalLatencyNs atomic.Int64
}

type providerMetrics struct {
	requests       atomic.Uint64
	errors         atomic.Uint64
	latencyTotalNs atomic.Int64
	latencyMaxNs   atomic.Int64
}

func NewMetricsPlugin() *MetricsPlugin {
	return &MetricsPlugin{}
}

func (m *MetricsPlugin) GetName() string { return "digitorn.metrics" }
func (m *MetricsPlugin) Cleanup() error  { return nil }

const metricsStartCtxKey ctxKey = 101
const metricsProviderCtxKey ctxKey = 102

func (m *MetricsPlugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	ctx.SetValue(metricsStartCtxKey, time.Now())
	prov := extractProviderName(req)
	ctx.SetValue(metricsProviderCtxKey, prov)
	return req, nil, nil
}

func (m *MetricsPlugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	startV := ctx.Value(metricsStartCtxKey)
	provV := ctx.Value(metricsProviderCtxKey)
	start, _ := startV.(time.Time)
	prov, _ := provV.(string)
	if prov == "" {
		prov = "unknown"
	}
	latencyNs := int64(0)
	if !start.IsZero() {
		latencyNs = time.Since(start).Nanoseconds()
	}
	m.totalRequests.Add(1)
	m.totalLatencyNs.Add(latencyNs)
	if bifrostErr != nil {
		m.totalErrors.Add(1)
	}

	pm := m.getOrCreate(prov)
	pm.requests.Add(1)
	pm.latencyTotalNs.Add(latencyNs)
	for {
		cur := pm.latencyMaxNs.Load()
		if latencyNs <= cur || pm.latencyMaxNs.CompareAndSwap(cur, latencyNs) {
			break
		}
	}
	if bifrostErr != nil {
		pm.errors.Add(1)
	}
	return resp, bifrostErr, nil
}

// getOrCreate returns the per-provider counter set, creating it once
// per provider via sync.Map.LoadOrStore. Hot path = one atomic.Load.
func (m *MetricsPlugin) getOrCreate(prov string) *providerMetrics {
	if v, ok := m.perProvider.Load(prov); ok {
		return v.(*providerMetrics)
	}
	fresh := &providerMetrics{}
	actual, _ := m.perProvider.LoadOrStore(prov, fresh)
	return actual.(*providerMetrics)
}

// Stats returns a snapshot of all counters. Safe to call concurrently.
type MetricsSnapshot struct {
	TotalRequests     uint64
	TotalErrors       uint64
	TotalAvgLatencyMs float64
	PerProvider       map[string]ProviderSnapshot
}

type ProviderSnapshot struct {
	Requests     uint64
	Errors       uint64
	AvgLatencyMs float64
	MaxLatencyMs float64
}

func (m *MetricsPlugin) Stats() MetricsSnapshot {
	total := m.totalRequests.Load()
	avg := 0.0
	if total > 0 {
		avg = float64(m.totalLatencyNs.Load()) / float64(total) / 1e6
	}
	out := MetricsSnapshot{
		TotalRequests:     total,
		TotalErrors:       m.totalErrors.Load(),
		TotalAvgLatencyMs: avg,
		PerProvider:       map[string]ProviderSnapshot{},
	}
	m.perProvider.Range(func(k, v any) bool {
		prov := k.(string)
		pm := v.(*providerMetrics)
		req := pm.requests.Load()
		pavg := 0.0
		if req > 0 {
			pavg = float64(pm.latencyTotalNs.Load()) / float64(req) / 1e6
		}
		out.PerProvider[prov] = ProviderSnapshot{
			Requests:     req,
			Errors:       pm.errors.Load(),
			AvgLatencyMs: pavg,
			MaxLatencyMs: float64(pm.latencyMaxNs.Load()) / 1e6,
		}
		return true
	})
	return out
}

// ---------- 3. Circuit breaker ----------

// cbRingSize is the fixed capacity of the per-provider failure timestamp
// ring buffer. Bigger = more accurate window count under bursty traffic,
// smaller = lower memory. 64 fits one full minute of failures at one
// per second per provider — well over any realistic threshold value.
const cbRingSize = 64

// cbState holds the per-provider circuit-breaker state. ALL fields are
// atomic so the hot path is lock-free. The struct lives in a sync.Map
// keyed by provider name.
type cbState struct {
	// openUntilNano is 0 when the circuit is closed. Otherwise it's the
	// UnixNano wall-clock at which the circuit transitions to half-open.
	openUntilNano atomic.Int64

	// halfOpenProbe is the half-open gate. When the cooldown expires
	// the FIRST request that observes it is the canary probe; we let
	// only one through. If the probe succeeds we close. If it fails we
	// reopen the cooldown.
	halfOpenProbe atomic.Bool

	// ring is the failure-timestamps ring buffer (UnixNano per slot,
	// 0 = empty slot). Writes use ringPos as the cursor; reads scan
	// the whole buffer to count failures within window.
	ring    [cbRingSize]atomic.Int64
	ringPos atomic.Uint32
}

// CircuitBreakerPlugin opens the circuit for a provider after N
// consecutive failures within Window, then keeps it open for OpenFor,
// short-circuiting requests with a typed error.
//
// Phase-2: every operation is lock-free on the hot path. The previous
// design held a sync.Mutex on EVERY PreLLMHook (open + closed paths),
// which serialised the entire process at high RPS. Verified zero-mutex
// hot path by plugins_concurrent_test.go under -race.
type CircuitBreakerPlugin struct {
	threshold int
	windowNs  int64
	openForNs int64

	// states: sync.Map[string]*cbState — one entry per seen provider.
	// First request for a provider does a LoadOrStore (one allocation,
	// one map write). All subsequent operations are atomic-only.
	states sync.Map

	totalOpens  atomic.Uint64
	totalShorts atomic.Uint64
}

func NewCircuitBreakerPlugin(threshold int, window, openFor time.Duration) *CircuitBreakerPlugin {
	if threshold <= 0 {
		threshold = 3
	}
	if window <= 0 {
		window = 30 * time.Second
	}
	if openFor <= 0 {
		// Defaults to 5s — short enough to feel responsive on transient
		// network blips (the common dev-mode failure), still long enough
		// to absorb 3 cascade attempts inside a 15s burst without
		// thrashing. Tune via DIGITORN_LLM_CB_OPEN_FOR in production —
		// 30s is a sane prod default.
		openFor = 5 * time.Second
	}
	return &CircuitBreakerPlugin{
		threshold: threshold,
		windowNs:  int64(window),
		openForNs: int64(openFor),
	}
}

func (c *CircuitBreakerPlugin) GetName() string { return "digitorn.circuit_breaker" }
func (c *CircuitBreakerPlugin) Cleanup() error  { return nil }

const cbProviderCtxKey ctxKey = 103
const cbHalfOpenProbeCtxKey ctxKey = 104

func (c *CircuitBreakerPlugin) getOrCreate(prov string) *cbState {
	if v, ok := c.states.Load(prov); ok {
		return v.(*cbState)
	}
	fresh := &cbState{}
	actual, _ := c.states.LoadOrStore(prov, fresh)
	return actual.(*cbState)
}

func (c *CircuitBreakerPlugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	prov := extractProviderName(req)
	ctx.SetValue(cbProviderCtxKey, prov)
	if prov == "" {
		return req, nil, nil
	}

	// Fast path: provider not seen yet = no state to inspect = closed.
	v, ok := c.states.Load(prov)
	if !ok {
		return req, nil, nil
	}
	s := v.(*cbState)

	openUntil := s.openUntilNano.Load()
	if openUntil == 0 {
		// Closed.
		return req, nil, nil
	}

	now := time.Now().UnixNano()
	if now < openUntil {
		// Still cooling down — short-circuit.
		c.totalShorts.Add(1)
		return req, shortCircuitOpen(prov), nil
	}

	// Cooldown expired. Half-open: let exactly one probe through.
	// CompareAndSwap from false→true wins the race; everyone else
	// gets short-circuited. This avoids stampede on recovery.
	if !s.halfOpenProbe.CompareAndSwap(false, true) {
		c.totalShorts.Add(1)
		return req, shortCircuitProbeInFlight(prov), nil
	}

	// We hold the probe slot. Pass through; PostLLMHook decides what to
	// do based on the outcome. Tag the ctx so the post-hook knows this
	// was the probe call and not a regular request.
	ctx.SetValue(cbHalfOpenProbeCtxKey, true)
	return req, nil, nil
}

func (c *CircuitBreakerPlugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	provV := ctx.Value(cbProviderCtxKey)
	prov, _ := provV.(string)
	if prov == "" {
		return resp, bifrostErr, nil
	}

	isProbe := false
	if pv := ctx.Value(cbHalfOpenProbeCtxKey); pv != nil {
		if b, ok := pv.(bool); ok {
			isProbe = b
		}
	}

	// The breaker only cares about PROVIDER HEALTH. A success OR a
	// client-side error (4xx: quota, auth, bad request) both prove the
	// upstream is reachable and answering — only 5xx / transport
	// failures mean the provider itself is unhealthy. Counting a 4xx
	// would let ONE user hitting their quota (429) trip the circuit for
	// every caller of that provider — the exact cascade we must avoid.
	if !cbIsHealthFailure(bifrostErr) {
		if isProbe {
			// Probe reached a responsive upstream → fully close + reset.
			s := c.getOrCreate(prov)
			s.openUntilNano.Store(0)
			s.halfOpenProbe.Store(false)
			for i := range s.ring {
				s.ring[i].Store(0)
			}
			s.ringPos.Store(0)
		}
		return resp, bifrostErr, nil
	}

	// Provider-health failure (5xx / transport).
	c.recordFailure(prov, isProbe)
	return resp, bifrostErr, nil
}

// cbIsHealthFailure decides whether a Bifrost error signals an UNHEALTHY
// provider (→ counts toward opening the circuit) versus a request that
// the upstream answered on its own terms (→ ignored by the breaker).
//
//   - nil error                         → not a failure (success)
//   - our own short-circuit errors      → ignored (never self-arm)
//   - 4xx (quota 429, auth 401/403,
//     bad-request 400/422, not-found)   → ignored: the request was wrong,
//     the provider is fine
//   - 5xx                               → failure (provider is erroring)
//   - status 0 (no HTTP response:
//     dial refused, TLS, timeout)       → failure (provider unreachable)
func cbIsHealthFailure(bifrostErr *schemas.BifrostError) bool {
	if bifrostErr == nil {
		return false
	}
	// Never let the breaker's own short-circuit responses re-arm it.
	if bifrostErr.Type != nil {
		switch *bifrostErr.Type {
		case "circuit_breaker_open", "circuit_breaker_probe_in_flight":
			return false
		}
	}
	if bifrostErr.StatusCode != nil {
		sc := *bifrostErr.StatusCode
		if sc >= 400 && sc < 500 {
			return false // client-side error — not a provider-health signal
		}
	}
	return true
}

// recordFailure registers a failure timestamp in the ring buffer and
// checks whether the window-count crossed the threshold. When the
// failure is the half-open probe, it directly re-opens the circuit
// regardless of count.
func (c *CircuitBreakerPlugin) recordFailure(prov string, isProbe bool) {
	s := c.getOrCreate(prov)
	now := time.Now().UnixNano()

	if isProbe {
		// Probe failed → re-open. Don't bother counting.
		s.openUntilNano.Store(now + c.openForNs)
		s.halfOpenProbe.Store(false)
		c.totalOpens.Add(1)
		return
	}

	// Write the new failure timestamp at the ring cursor.
	pos := s.ringPos.Add(1) % cbRingSize
	s.ring[pos].Store(now)

	// Count failures within the sliding window. Atomic loads only;
	// runs fully lock-free, ~50 ns for 64 slots.
	cutoff := now - c.windowNs
	count := 0
	for i := 0; i < cbRingSize; i++ {
		if s.ring[i].Load() >= cutoff {
			count++
		}
	}
	if count >= c.threshold {
		// Open the circuit. Use CAS to count opens correctly: only
		// transition closed → open counts as a new "open" event.
		prevOpen := s.openUntilNano.Load()
		if prevOpen == 0 || now >= prevOpen {
			c.totalOpens.Add(1)
		}
		s.openUntilNano.Store(now + c.openForNs)
	}
}

// shortCircuitOpen builds the typed Bifrost error for an open circuit.
// Allocated lazily — only when we actually short-circuit.
func shortCircuitOpen(prov string) *schemas.LLMPluginShortCircuit {
	typ := "circuit_breaker_open"
	return &schemas.LLMPluginShortCircuit{
		Error: &schemas.BifrostError{
			IsBifrostError: true,
			Type:           &typ,
			Error:          &schemas.ErrorField{Message: "circuit breaker open for provider " + prov},
		},
	}
}

// shortCircuitProbeInFlight returns the error for "another half-open
// probe is already testing the upstream, please retry in a moment".
// Different type so callers can distinguish "we're feeling out the
// upstream, don't pile on" from "the upstream is dead, give up".
func shortCircuitProbeInFlight(prov string) *schemas.LLMPluginShortCircuit {
	typ := "circuit_breaker_probe_in_flight"
	return &schemas.LLMPluginShortCircuit{
		Error: &schemas.BifrostError{
			IsBifrostError: true,
			Type:           &typ,
			Error:          &schemas.ErrorField{Message: "circuit breaker half-open probe in flight for provider " + prov + ", retry shortly"},
		},
	}
}

// Reset clears the circuit breaker state for one provider. Used by the
// operator-facing reset trigger when the upstream is known-recovered
// and we don't want to wait for the cooldown. Idempotent.
//
// Phase-2 unblock: previously the ONLY way to clear the breaker was to
// kill the worker-llm process — exactly the failure mode the user hit
// after the master-key swap. Now an in-process reset is one call.
func (c *CircuitBreakerPlugin) Reset(prov string) {
	v, ok := c.states.Load(prov)
	if !ok {
		return
	}
	s := v.(*cbState)
	s.openUntilNano.Store(0)
	s.halfOpenProbe.Store(false)
	for i := range s.ring {
		s.ring[i].Store(0)
	}
	s.ringPos.Store(0)
}

// ResetAll clears the breaker state for every known provider. Used at
// daemon boot if the operator wants to start with a guaranteed-clean
// slate, or via an admin signal handler.
func (c *CircuitBreakerPlugin) ResetAll() {
	c.states.Range(func(k, _ any) bool {
		c.Reset(k.(string))
		return true
	})
}

type CircuitBreakerSnapshot struct {
	OpenProviders []string
	TotalOpens    uint64
	TotalShorts   uint64
}

func (c *CircuitBreakerPlugin) Stats() CircuitBreakerSnapshot {
	now := time.Now().UnixNano()
	open := []string{}
	c.states.Range(func(k, v any) bool {
		prov := k.(string)
		s := v.(*cbState)
		until := s.openUntilNano.Load()
		if until > 0 && now < until {
			open = append(open, prov)
		}
		return true
	})
	return CircuitBreakerSnapshot{
		OpenProviders: open,
		TotalOpens:    c.totalOpens.Load(),
		TotalShorts:   c.totalShorts.Load(),
	}
}

// ---------- common helpers ----------

func extractProviderName(req *schemas.BifrostRequest) string {
	if req == nil {
		return ""
	}
	if req.ChatRequest != nil {
		return string(req.ChatRequest.Provider)
	}
	if req.EmbeddingRequest != nil {
		return string(req.EmbeddingRequest.Provider)
	}
	if req.TextCompletionRequest != nil {
		return string(req.TextCompletionRequest.Provider)
	}
	return ""
}

// PluginSet groups the three plugins for easy wiring.
type PluginSet struct {
	Audit          *AuditPlugin
	Metrics        *MetricsPlugin
	CircuitBreaker *CircuitBreakerPlugin
}

// AsLLMPlugins returns the slice format Bifrost expects in BifrostConfig.
func (p *PluginSet) AsLLMPlugins() []schemas.LLMPlugin {
	out := []schemas.LLMPlugin{}
	if p.Audit != nil {
		out = append(out, p.Audit)
	}
	if p.Metrics != nil {
		out = append(out, p.Metrics)
	}
	if p.CircuitBreaker != nil {
		out = append(out, p.CircuitBreaker)
	}
	return out
}

// NewDefaultPluginSet builds the standard digitorn V1 set with hard-coded
// CB defaults. Kept for back-compat with tests / callers that don't tune
// CB. New code should use NewPluginSet to inject CB params from config.
func NewDefaultPluginSet(logger *slog.Logger, auditEnabled bool) *PluginSet {
	return NewPluginSet(logger, auditEnabled, 0, 0, 0)
}

// NewPluginSet builds the plugin set with explicit CB params. Pass 0
// for any of (cbThreshold, cbWindow, cbOpenFor) to use the safe defaults
// inside NewCircuitBreakerPlugin (3 / 30s / 5s).
func NewPluginSet(logger *slog.Logger, auditEnabled bool, cbThreshold int, cbWindow, cbOpenFor time.Duration) *PluginSet {
	return &PluginSet{
		Audit:          NewAuditPlugin(logger, auditEnabled),
		Metrics:        NewMetricsPlugin(),
		CircuitBreaker: NewCircuitBreakerPlugin(cbThreshold, cbWindow, cbOpenFor),
	}
}

// Sanity error so the package is allowed to reference errors stdlib if
// we add explicit ones later.
var _ = errors.New
