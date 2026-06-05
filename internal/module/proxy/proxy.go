// Package proxy implements WorkerProxyModule : a domainmodule.Module
// that forwards Invoke() to a worker subprocess via gRPC. The
// servicebus registers it just like any in-process module, so the
// runtime never sees the difference.
//
// One ProxyModule per (moduleID, workerKind) pair. The proxy holds
// the cached manifest (fetched at boot via service.Manifests RPC)
// so domainmodule.Module.Manifest() is O(1) without a network call.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/module/service"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// Picker is the minimal surface the proxy needs from a worker pool :
// hand me a healthy connection (or an error). *worker.Manager
// satisfies this interface, but tests can substitute a mock.
type Picker interface {
	Pick(ctx context.Context, kind worker.Kind) (worker.Conn, error)
}

// ProxyModule is a domainmodule.Module whose Invoke() calls a worker
// subprocess via gRPC instead of running code in-process. Stateless
// on the call path except for the cached manifest and the logger.
type ProxyModule struct {
	manifest domainmodule.Manifest
	kind     worker.Kind
	picker   Picker
	logger   *slog.Logger

	// invokeTimeout caps each individual Invoke RPC. If 0, the
	// caller's ctx deadline applies as-is. Defaults to 60s — long
	// enough for heavy LSP / MCP calls, short enough that a wedged
	// worker doesn't hold a runtime turn forever.
	invokeTimeout time.Duration
}

// Options configures a ProxyModule. ModuleID + Kind are required ;
// the rest defaults sensibly.
type Options struct {
	// ModuleID is the module the worker hosts (e.g. "lsp").
	ModuleID string

	// Kind is the worker pool to dispatch to (e.g. "lsp-pool").
	Kind worker.Kind

	// Picker grabs a worker connection. Required.
	Picker Picker

	// InvokeTimeout caps each Invoke RPC. 0 ≡ 60s.
	InvokeTimeout time.Duration

	// Logger for transport-level errors. Defaults to slog.Default.
	Logger *slog.Logger
}

// New constructs a ProxyModule. It fetches the worker's manifest list
// via Manifests RPC and caches the entry for ModuleID locally. Fails
// if the worker does NOT actually host ModuleID — config drift
// between daemon (workers.pools[].modules) and worker
// (DIGITORN_WORKER_MODULES) is a startup bug, not a runtime one, so
// we surface it loudly.
func New(ctx context.Context, opts Options) (*ProxyModule, error) {
	if opts.ModuleID == "" {
		return nil, fmt.Errorf("proxy: empty ModuleID")
	}
	if opts.Kind == "" {
		return nil, fmt.Errorf("proxy: empty Kind")
	}
	if opts.Picker == nil {
		return nil, fmt.Errorf("proxy: nil Picker")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := opts.InvokeTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	conn, err := opts.Picker.Pick(ctx, opts.Kind)
	if err != nil {
		return nil, fmt.Errorf("proxy: pick worker for kind=%q: %w", opts.Kind, err)
	}

	out := new(service.ManifestsResponse)
	if err := conn.GRPC().Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodManifests,
		&service.ManifestsRequest{}, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		return nil, fmt.Errorf("proxy: fetch manifests from kind=%q: %w", opts.Kind, err)
	}

	var manifest *domainmodule.Manifest
	for i := range out.Modules {
		if out.Modules[i].ID == opts.ModuleID {
			manifest = &out.Modules[i]
			break
		}
	}
	if manifest == nil {
		hosted := make([]string, 0, len(out.Modules))
		for _, m := range out.Modules {
			hosted = append(hosted, m.ID)
		}
		return nil, fmt.Errorf("proxy: worker pool %q does not host module %q (hosted: %v)",
			opts.Kind, opts.ModuleID, hosted)
	}

	return &ProxyModule{
		manifest:      *manifest,
		kind:          opts.Kind,
		picker:        opts.Picker,
		logger:        logger,
		invokeTimeout: timeout,
	}, nil
}

// Manifest returns the cached manifest fetched at construction.
// O(1), no network.
func (p *ProxyModule) Manifest() domainmodule.Manifest { return p.manifest }

// Kind returns the worker pool ID this proxy dispatches to. Useful
// for diagnostics — type-assert any servicebus.Module to *ProxyModule
// and call .Kind() to know which pool serves it.
func (p *ProxyModule) Kind() worker.Kind { return p.kind }

// Init is a no-op : the worker subprocess owns the module's lifecycle.
// The daemon-side proxy has nothing to initialise.
func (p *ProxyModule) Init(ctx context.Context, cfg map[string]any) error { return nil }

// Start is a no-op. The worker is already up by the time this proxy
// was constructed (otherwise Manifests would have failed).
func (p *ProxyModule) Start(ctx context.Context) error { return nil }

// Stop is a no-op : stopping the worker subprocess is the daemon's
// worker.Manager responsibility, NOT this proxy's. Calling Stop on
// the proxy must never tear down the underlying worker — multiple
// modules may share one worker.
func (p *ProxyModule) Stop(ctx context.Context) error { return nil }

// Invoke forwards the call to a worker via gRPC. The error semantics
// match in-process modules :
//   - tool.Result.Success=false carries module-level errors (the
//     worker already shaped them ; we pass through verbatim)
//   - returned error is reserved for transport / pool failures so
//     the runtime's retry layer can tell them apart
func (p *ProxyModule) Invoke(ctx context.Context, toolName string, params []byte) (tool.Result, error) {
	if p.invokeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.invokeTimeout)
		defer cancel()
	}

	conn, err := p.picker.Pick(ctx, p.kind)
	if err != nil {
		return tool.Result{
			Success: false,
			Error:   fmt.Sprintf("no worker available for %s: %v", p.kind, err),
		}, fmt.Errorf("proxy: pick %s: %w", p.kind, err)
	}

	req := &service.InvokeRequest{
		ModuleID:  p.manifest.ID,
		ToolName:  toolName,
		Params:    params,
		RequestID: newRequestID(),
	}
	if id, ok := tool.IdentityFromContext(ctx); ok {
		req.AppID, req.SessionID, req.UserID, req.AgentID = id.AppID, id.SessionID, id.UserID, id.AgentID
	}
	if dl, ok := ctx.Deadline(); ok {
		req.DeadlineMs = time.Until(dl).Milliseconds()
	}

	out := new(service.InvokeResponse)
	err = conn.GRPC().Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodInvoke,
		req, out,
		grpc.CallContentSubtype(service.CodecName),
	)
	if err != nil {
		// Transport / framework error : worker died, network
		// glitch, ctx deadline. Caller's retry layer decides.
		return tool.Result{
			Success: false,
			Error:   fmt.Sprintf("worker invoke failed: %v", err),
		}, fmt.Errorf("proxy: invoke %s.%s: %w", p.manifest.ID, toolName, err)
	}

	// Worker-shaped result : pass through (Success may be true or
	// false depending on what the module returned).
	return out.Result, nil
}

// newRequestID returns a 16-hex-character random id for tracing.
// Crypto-strength is overkill ; we use crypto/rand only because
// math/rand needs seeding and the cost difference is negligible.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Compile-time check : the proxy satisfies the domain interface.
var (
	_ domainmodule.Module = (*ProxyModule)(nil)
	_ error               = (*errNoHealthyWorker)(nil)
)

// errNoHealthyWorker is a typed error a Picker may return to signal
// "the pool exists but currently has no healthy worker". The proxy
// wraps it but also surfaces it via errors.Is so the caller can tell
// it apart from gRPC transport errors.
type errNoHealthyWorker struct{ kind worker.Kind }

func (e *errNoHealthyWorker) Error() string {
	return fmt.Sprintf("proxy: no healthy worker for kind=%q", e.kind)
}

// ErrNoHealthyWorker is a typed alias for tests / retry classification.
var ErrNoHealthyWorker = errors.New("proxy: no healthy worker")
