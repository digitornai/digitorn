package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/config"
	"github.com/mbathepaul/digitorn/internal/embeddings"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/module/proxy"
	"github.com/mbathepaul/digitorn/internal/runtime/contextcompact"
	"github.com/mbathepaul/digitorn/internal/runtime/contextsvc"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/tokenizer"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// startWorkers builds the in-process clients for every configured worker pool
// and launches their subprocesses IN THE BACKGROUND. It is a HARD invariant
// that NO worker ever blocks daemon startup : the client objects are wired
// synchronously (they only need the manager and pick a ready instance at call
// time), while the actual pool spawn — which waits up to start_timeout for an
// instance to report ready, and crash-loops a broken binary — runs off the
// boot path. A worker that is slow or failing can no longer hold the whole
// daemon hostage on its start_timeout ; calls that need it fail (and retry)
// until it recovers. Setup errors here are the cheap, synchronous kind (binary
// not found) ; they are logged, never fatal.
func (d *Daemon) startWorkers(ctx context.Context) {
	if d.cfg.Workers.LLM.Count > 0 {
		if err := d.startLLMWorker(ctx); err != nil {
			d.logger.Error("daemon: LLM worker setup failed; LLM calls disabled",
				slog.String("err", err.Error()))
		}
	}
	if d.cfg.Workers.Embeddings.Count > 0 {
		if err := d.startEmbeddingsWorker(ctx); err != nil {
			d.logger.Error("daemon: embeddings worker setup failed; semantic search disabled",
				slog.String("err", err.Error()))
		}
	}
	if d.cfg.Workers.Tokenizer.Count > 0 {
		if err := d.startTokenizerWorker(ctx); err != nil {
			d.logger.Error("daemon: tokenizer worker setup failed; context occupancy keeps the provider anchor",
				slog.String("err", err.Error()))
		}
	}
}

// spawnPoolAsync launches a worker pool in a background goroutine so a slow or
// crash-looping worker never blocks daemon startup. The pool registers with the
// manager once its first instance is ready (or after its start_timeout when it
// is failing) ; onReady runs only on a clean spawn. The spawn honours ctx, so a
// shutdown mid-start aborts it.
func (d *Daemon) spawnPoolAsync(ctx context.Context, spec worker.Spec, onReady func()) {
	go func() {
		if err := d.workerMgr.Spawn(ctx, spec); err != nil {
			d.logger.Error("daemon: worker pool spawn failed in background; calls needing it fail until it recovers",
				slog.String("kind", string(spec.Kind)),
				slog.String("err", err.Error()))
			return
		}
		if onReady != nil {
			onReady()
		}
	}()
}

// startEmbeddingsWorker spawns the embeddings worker pool and wires
// an *embeddings.Client into the daemon so the context_builder
// pipeline can opt into semantic search. Doc reference :
// docs-site/language/04-tools.md "Semantic search".
func (d *Daemon) startEmbeddingsWorker(ctx context.Context) error {
	cfg := d.cfg.Workers.Embeddings
	binary, err := resolveWorkerBinary(cfg.BinaryPath, "digitorn-worker-embeddings")
	if err != nil {
		return fmt.Errorf("locate binary: %w", err)
	}

	env := map[string]string{}
	if cfg.Backend != "" {
		env["DIGITORN_EMBED_BACKEND"] = cfg.Backend
	}
	if cfg.ModelDir != "" {
		env["DIGITORN_EMBED_MODEL_DIR"] = cfg.ModelDir
	}
	if cfg.Quantized {
		env["DIGITORN_EMBED_QUANTIZED"] = "1"
	}
	if cfg.Device != "" {
		env["DIGITORN_EMBED_DEVICE"] = cfg.Device
	}

	// Client first, synchronously : it only needs the manager (picks a ready
	// instance at call time), and BuildFor falls back to keyword search whenever
	// a semantic attach errors — so wiring it before the worker is ready is
	// safe and lets a recovered worker light up semantic search with no reboot.
	client := embeddings.NewClient(d.workerMgr)
	if cfg.ClientTimeout > 0 {
		client = client.WithTimeout(cfg.ClientTimeout)
	}
	d.embeddingsClient = client

	// Spawn off the boot path — a non-ONNX or slow worker must never block the
	// daemon on its start_timeout (this was the 90s startup stall).
	d.spawnPoolAsync(ctx, worker.Spec{
		Kind:         embeddings.Kind,
		Binary:       binary,
		Count:        cfg.Count,
		Env:          env,
		StartTimeout: cfg.StartTimeout,
		StopTimeout:  cfg.StopTimeout,
		BackoffMin:   cfg.BackoffMin,
		BackoffMax:   cfg.BackoffMax,
		MaxFailures:  cfg.MaxFailures,
		HealthEvery:  cfg.HealthEvery,
	}, func() {
		d.logger.Info("daemon: embeddings worker pool ready",
			slog.Int("count", cfg.Count),
			slog.String("binary", binary),
			slog.String("backend", cfg.Backend))
	})
	return nil
}

// startTokenizerWorker spawns the tokenizer worker pool and wires a
// *tokenizer.Client into the daemon so the ContextService can refine the
// between-anchor occupancy delta in the background (CTX-7.3). The worker is
// purely additive : if it never comes up, the provider usage anchor remains the
// occupancy gauge (exact at every turn boundary), so a missing/slow/crashing
// tokenizer worker can never degrade correctness or block the loop.
func (d *Daemon) startTokenizerWorker(ctx context.Context) error {
	cfg := d.cfg.Workers.Tokenizer
	binary, err := resolveWorkerBinary(cfg.BinaryPath, "digitorn-worker-tokenizer")
	if err != nil {
		return fmt.Errorf("locate binary: %w", err)
	}

	// Client first, synchronously : it only needs the manager (picks a ready
	// instance at call time). Wiring it before the pool is ready is safe — a
	// call with no ready worker errors and the caller keeps the anchor.
	client := tokenizer.NewClient(d.workerMgr)
	if cfg.ClientTimeout > 0 {
		client = client.WithTimeout(cfg.ClientTimeout)
	}
	d.tokenizerClient = client

	// Wire the background Context Service : it recounts the EXACT context size
	// off the turn loop and emits EventContextTokens. The engine's ContextTouch
	// (set in buildEngine) signals it ; until this Store, ContextTouch is a
	// no-op and the provider anchor keeps the gauge exact per turn.
	bg := contextsvc.NewBackground(client, &contextViewSource{d: d}, d.onContextRecomputed)
	bg.Start(4)
	d.contextBG.Store(bg)

	d.spawnPoolAsync(ctx, worker.Spec{
		Kind:         tokenizer.Kind,
		Binary:       binary,
		Count:        cfg.Count,
		StartTimeout: cfg.StartTimeout,
		StopTimeout:  cfg.StopTimeout,
		BackoffMin:   cfg.BackoffMin,
		BackoffMax:   cfg.BackoffMax,
		MaxFailures:  cfg.MaxFailures,
		HealthEvery:  cfg.HealthEvery,
	}, func() {
		d.logger.Info("daemon: tokenizer worker pool ready",
			slog.Int("count", cfg.Count),
			slog.String("binary", binary))
	})
	return nil
}

// touchContext is the engine's non-blocking signal to recount the EXACT
// context size. No-op until the background service is wired (tokenizer worker
// up) ; never blocks.
func (d *Daemon) touchContext(sessionID string) {
	if bg := d.contextBG.Load(); bg != nil {
		bg.Touch(sessionID)
	}
}

// onContextRecomputed receives the EXACT count from the background service and
// persists it as EventContextTokens — which sets the occupancy gauge, streams
// to clients via the bridge, and feeds context_pressure. Skips a no-change
// recompute so the durable log isn't spammed. Runs on a worker goroutine, off
// the turn loop.
func (d *Daemon) onContextRecomputed(r contextsvc.Result) {
	if r.SessionID == "" || r.Total <= 0 {
		return
	}
	st, err := d.sessionStore.State(r.SessionID)
	if err != nil || st == nil {
		return
	}
	snap := st.Snapshot()

	// Calibrate the raw tokenizer (tiktoken) count to THIS session's provider
	// REAL count — provider-agnostic, learned from the provider's own usage. A
	// fresh provider anchor (turn just ended) → the displayed total is the
	// provider's EXACT count ; otherwise (between turns / post-compaction) apply
	// the learned ratio. The breakdown is scaled so the buckets still sum to it.
	oldRatio := 0.0
	if d.contextTracker != nil {
		if old, ok := d.contextTracker.Get(r.SessionID); ok {
			oldRatio = old.TokRatio
		}
	}
	total, newRatio := contextsvc.CalibrateTotal(r.Total, snap.ContextProviderTokens, oldRatio)
	system, tools, messages := r.System, r.Tools, r.Messages
	if r.Total > 0 && total != r.Total {
		f := float64(total) / float64(r.Total)
		system = int(math.Round(float64(r.System) * f))
		tools = int(math.Round(float64(r.Tools) * f))
		messages = int(math.Round(float64(r.Messages) * f))
	}

	// CTX-V : refresh the in-memory context variable IMMEDIATELY with the fresh
	// CALIBRATED total + breakdown — it leads the durable projection, so the
	// per-round guard / hooks see the real context without the event round-trip.
	if d.contextTracker != nil {
		view := contextsvc.ViewFromSnapshot(snap, d.brainFor(snap.AppID)).
			WithExactTotal(total, system, tools, messages)
		if old, ok := d.contextTracker.Get(r.SessionID); ok && old.EstimateRatio > 0 {
			view.EstimateRatio = old.EstimateRatio
		}
		view.TokRatio = newRatio
		d.contextTracker.Put(r.SessionID, view)
	}
	// Emit on EVERY recount so the client gauge updates at the SAME frequency as
	// the recount — 1 recount = 1 client update, nothing decouples the two.
	// Append (write-behind), NOT AppendDurable : the gauge is recomputable, so it
	// must never pay a per-recount fsync — under the per-event touches that would
	// be an fsync storm. Append still projects the value and notifies subscribers
	// IMMEDIATELY in-memory (client gets it now), the disk write coalesces in the
	// background. The window denominator is resolved the way compaction does
	// (configured context.max_tokens, else the model's documented window) so the
	// gauge's denominator matches the daemon's pressure denominator exactly.
	res := contextsvc.Resolve(snap, d.brainFor(snap.AppID))
	_, _ = d.sessionStore.Append(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventContextTokens,
		SessionID: r.SessionID,
		CtxTokens: &sessionstore.ContextTokensPayload{
			Total:    total,
			System:   system,
			Tools:    tools,
			Messages: messages,
			Window:   res.Window,
			Limit:    res.MaxTokens,
		},
	})
}

// brainFor returns the entry agent's brain for an app (the one whose context
// config drives window resolution). Zero-value brain when the app/agent is
// unknown — Resolve then falls back to the model default table.
func (d *Daemon) brainFor(appID string) schema.Brain {
	if d.appMgr == nil || appID == "" {
		return schema.Brain{}
	}
	app, err := d.appMgr.Get(context.Background(), appID)
	if err != nil || app == nil || app.Definition == nil || len(app.Definition.Agents) == 0 {
		return schema.Brain{}
	}
	return app.Definition.Agents[0].Brain
}

// recordContextRatio stores the engine's self-calibrated provider/estimate
// ratio for a session so the next turn's pre-send budget guard targets the
// model's REAL token count. Merges into the existing tracked view (preserving
// the exact counts), last-wins — a benign race that just re-calibrates.
func (d *Daemon) recordContextRatio(sessionID string, ratio float64) {
	if d.contextTracker == nil || sessionID == "" || ratio <= 0 {
		return
	}
	v, _ := d.contextTracker.Get(sessionID)
	v.EstimateRatio = ratio
	d.contextTracker.Put(sessionID, v)
}

// ctxParts is the engine-recorded system prompt + tool schemas for a session,
// stashed at request-build so the background recount can attribute the
// system/tools buckets (they live in the assembled request, not the session).
type ctxParts struct {
	system []string
	tools  []string
}

// recordContextParts stashes the build-time system+tools texts and signals a
// recount. Called by the engine (ContextRecordParts) on the hot build path —
// must stay non-blocking (a map store + a Touch).
func (d *Daemon) recordContextParts(sessionID string, system, tools []string) {
	if sessionID == "" {
		return
	}
	d.ctxParts.Store(sessionID, ctxParts{system: system, tools: tools})
	d.touchContext(sessionID)
}

// contextViewSource resolves the current EXACT context split for a session :
// messages from the live (compacted) projection, system + tools from the
// engine's last build-time stash. Reads are lock-free.
type contextViewSource struct{ d *Daemon }

func (v *contextViewSource) ContextView(sessionID string) (contextsvc.View, bool) {
	st, err := v.d.sessionStore.State(sessionID)
	if err != nil || st == nil {
		return contextsvc.View{}, false
	}
	snap := st.Snapshot()
	msgs := snap.Messages
	if snap.ContextCompaction != nil && snap.ContextCompaction.CutoffSeq > 0 {
		msgs = contextcompact.ApplyView(msgs, snap.ContextCompaction.CutoffSeq, snap.ContextCompaction.Summary)
	}
	messages := make([]string, 0, len(msgs))
	for i := range msgs {
		messages = append(messages, plainText(msgs[i]))
	}
	view := contextsvc.View{Messages: messages}
	if p, ok := v.d.ctxParts.Load(sessionID); ok {
		parts := p.(ctxParts)
		view.System = parts.system
		view.Tools = parts.tools
	}
	if len(view.System) == 0 && len(view.Tools) == 0 && len(view.Messages) == 0 {
		return contextsvc.View{}, false
	}
	view.Provider, view.Model = v.brain(snap.AppID)
	return view, true
}

func (v *contextViewSource) brain(appID string) (provider, model string) {
	b := v.d.brainFor(appID)
	return b.Provider, b.Model
}

func (d *Daemon) startLLMWorker(ctx context.Context) error {
	cfg := d.cfg.Workers.LLM
	binary, err := resolveWorkerBinary(cfg.BinaryPath, "digitorn-worker-llm")
	if err != nil {
		return fmt.Errorf("locate binary: %w", err)
	}

	env := map[string]string{}
	if cfg.GatewayURL != "" {
		env["DIGITORN_LLM_GATEWAY_URL"] = cfg.GatewayURL
	}
	if cfg.Concurrency > 0 {
		env["DIGITORN_LLM_CONCURRENCY"] = fmt.Sprintf("%d", cfg.Concurrency)
	}
	if cfg.BufferSize > 0 {
		env["DIGITORN_LLM_BUFFER"] = fmt.Sprintf("%d", cfg.BufferSize)
	}
	// Phase 6: per-provider overrides serialised as JSON. Empty maps
	// stay unset so the worker keeps its global defaults.
	if len(cfg.PerProviderConcurrency) > 0 {
		if b, err := json.Marshal(cfg.PerProviderConcurrency); err == nil {
			env["DIGITORN_LLM_PER_PROVIDER_CONCURRENCY"] = string(b)
		}
	}
	if len(cfg.PerProviderBufferSize) > 0 {
		if b, err := json.Marshal(cfg.PerProviderBufferSize); err == nil {
			env["DIGITORN_LLM_PER_PROVIDER_BUFFER"] = string(b)
		}
	}

	// Client first, synchronously, so buildEngine wires a non-nil LLM client.
	// The client picks a ready instance at call time, so it does not need the
	// pool to be up yet — the spawn runs in the background below.
	client, err := llm.NewClient(llm.ClientConfig{
		Manager: d.workerMgr,
		Kind:    "llm",
		Retries: cfg.ClientRetries,
		Timeout: cfg.ClientTimeout,
		Logger:  d.logger,
	})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	d.llmClient = client

	// Spawn off the boot path — never block the daemon waiting for the LLM
	// worker (or its gateway) to come up.
	d.spawnPoolAsync(ctx, worker.Spec{
		Kind:         "llm",
		Binary:       binary,
		Count:        cfg.Count,
		Env:          env,
		StartTimeout: cfg.StartTimeout,
		StopTimeout:  cfg.StopTimeout,
		BackoffMin:   cfg.BackoffMin,
		BackoffMax:   cfg.BackoffMax,
		MaxFailures:  cfg.MaxFailures,
		HealthEvery:  cfg.HealthEvery,
	}, func() {
		d.logger.Info("daemon: LLM worker pool ready",
			slog.Int("count", cfg.Count),
			slog.String("binary", binary),
			slog.String("gateway_url", cfg.GatewayURL))
	})
	return nil
}

// startWorkerPools spawns every cfg.Workers.Pools entry, then registers a
// ProxyModule per (pool, module) pair in the servicebus. Returns the
// list of module IDs that are now served by a worker — the caller uses
// it to skip the matching in-proc instances via StartExcept.
//
// All failures are logged but never fatal : a missing worker pool
// degrades to the daemon serving whatever in-proc modules are
// available, instead of refusing to boot.
func (d *Daemon) startWorkerPools(ctx context.Context) []string {
	pools := d.cfg.Workers.Pools
	if len(pools) == 0 {
		return nil
	}
	// The module→pool mapping is static config, so the workerised module ids are
	// known synchronously (the caller skips their in-proc instances). The actual
	// subprocess spawn + proxy wiring run in the BACKGROUND so a slow or broken
	// pool worker never blocks daemon startup. Trade-off vs the old synchronous
	// path : a pool that never comes up leaves its modules unavailable (no
	// in-proc fallback) — consistent with the "no worker blocks startup" rule ;
	// the failure is logged. servicebus.Register is mutex-guarded, so the late
	// proxy registration from the goroutine is race-free.
	var workerised []string
	for _, p := range pools {
		workerised = append(workerised, p.Modules...)
		pool := p
		go func() {
			if err := d.startOneWorkerPool(ctx, pool); err != nil {
				d.logger.Error("daemon: worker pool failed in background; its modules stay unavailable until it recovers",
					slog.String("pool_id", pool.ID),
					slog.Any("modules", pool.Modules),
					slog.String("err", err.Error()),
				)
			}
		}()
	}
	return workerised
}

// startOneWorkerPool spawns the pool's subprocesses and creates one
// ProxyModule per module, registered with the servicebus.
func (d *Daemon) startOneWorkerPool(ctx context.Context, p config.WorkerPool) error {
	if p.ID == "" {
		return errors.New("worker pool : empty id")
	}
	if len(p.Modules) == 0 {
		return fmt.Errorf("worker pool %q : no modules declared", p.ID)
	}

	binary, err := resolveWorkerBinary(p.BinaryPath, "digitorn-worker")
	if err != nil {
		return fmt.Errorf("worker pool %q : locate binary: %w", p.ID, err)
	}

	// Env : explicit per-pool env merged with the DIGITORN_WORKER_MODULES
	// list. The pool's own Env entries take precedence so the operator
	// can override defaults (DIGITORN_MODULE_FOO_CONFIG etc.).
	env := make(map[string]string, len(p.Env)+2)
	env["DIGITORN_WORKER_MODULES"] = strings.Join(p.Modules, ",")
	// Opt-in AF_UNIX transport : hand the pool a socket DIRECTORY so each
	// instance binds a unique socket inside it (no collision at Count>1).
	// Set before the operator's Env so an explicit DIGITORN_WORKER_BIND wins.
	if p.Transport == "unix" {
		sockDir := filepath.Join(os.TempDir(), "digitorn-sockets", p.ID) + string(os.PathSeparator)
		env[worker.EnvBindKey] = "unix:" + sockDir
	}
	for k, v := range p.Env {
		env[k] = v
	}

	count := p.Count
	if count <= 0 {
		count = 1
	}
	startTO := p.StartTimeout
	if startTO <= 0 {
		startTO = 15 * time.Second
	}

	if err := d.workerMgr.Spawn(ctx, worker.Spec{
		Kind:         worker.Kind(p.ID),
		Binary:       binary,
		Count:        count,
		Env:          env,
		StartTimeout: startTO,
		StopTimeout:  p.StopTimeout,
		BackoffMin:   p.BackoffMin,
		BackoffMax:   p.BackoffMax,
		MaxFailures:  p.MaxFailures,
	}); err != nil {
		return fmt.Errorf("worker pool %q : spawn: %w", p.ID, err)
	}

	// Build one ProxyModule per declared module, register with the
	// servicebus. New() round-trips Manifests() to catch config drift
	// between the pool's Modules list and what the worker actually
	// hosts.
	invokeTO := p.InvokeTimeout
	for _, modID := range p.Modules {
		px, err := proxy.New(ctx, proxy.Options{
			ModuleID:      modID,
			Kind:          worker.Kind(p.ID),
			Picker:        d.workerMgr,
			InvokeTimeout: invokeTO,
			Logger:        d.logger,
		})
		if err != nil {
			d.logger.Error("daemon: proxy create failed",
				slog.String("pool_id", p.ID),
				slog.String("module_id", modID),
				slog.String("err", err.Error()),
			)
			continue
		}
		if err := d.bus.Register(px); err != nil {
			d.logger.Error("daemon: bus register proxy failed",
				slog.String("module_id", modID),
				slog.String("err", err.Error()),
			)
			continue
		}
		d.logger.Info("daemon: worker-hosted module ready",
			slog.String("pool_id", p.ID),
			slog.String("module_id", modID),
			slog.Int("workers", count),
		)
	}
	return nil
}

// resolveWorkerBinary returns an absolute path to the worker binary. It
// searches : (1) the explicit config path, (2) alongside the daemon
// executable, (3) the OS PATH.
func resolveWorkerBinary(explicit, defaultName string) (string, error) {
	if explicit != "" {
		abs, _ := filepath.Abs(explicit)
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
		return "", fmt.Errorf("configured binary %s not found", explicit)
	}
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		name := defaultName
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath(defaultName); err == nil {
		return p, nil
	}
	return "", errors.New("worker binary not found in config, alongside daemon, or in PATH")
}
