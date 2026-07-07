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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/module/service"
	"github.com/digitornai/digitorn/internal/worker"
	pkgmodule "github.com/digitornai/digitorn/pkg/module"
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

	// authResolver, when set (the mcp proxy only), resolves a per-user
	// credential before each call and either injects it (req.AuthContext) or
	// short-circuits the call with a blocking needs-auth result. nil disables it.
	authResolver service.AuthResolver
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

	// AuthResolver resolves per-user MCP credentials. Set only for the mcp
	// module; nil for every other worker-hosted module.
	AuthResolver service.AuthResolver
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
		authResolver:  opts.AuthResolver,
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
	req.AppDir = pkgmodule.AppDir(ctx)
	// Forward the app's resolved per-module config so the worker-hosted
	// module reads its app-specific configuration on this call.
	if cfg := pkgmodule.ModuleConfigFrom(ctx); len(cfg) > 0 {
		if b, err := json.Marshal(cfg); err == nil {
			req.Config = b
		}
	}
	// Resolve the per-user MCP credential daemon-side (mcp proxy only). The
	// resolver either injects a token (req.AuthContext) or returns a challenge,
	// in which case the call is blocked here and never reaches the worker — so
	// the worker never mints auth URLs and never sees a missing-token decision.
	if p.authResolver != nil && p.manifest.ID == "mcp" && req.UserID != "" {
		ac, challenge, rerr := p.authResolver.ResolveAuth(ctx, req.UserID, req.AppID, req.ModuleID, toolName)
		if rerr != nil {
			return tool.Result{Success: false, Error: "auth resolution failed: " + rerr.Error()}, nil
		}
		if challenge != nil {
			return needsAuthResult(challenge), nil
		}
		req.AuthContext = ac
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

// Tools fetches the module's runtime tools from the worker, forwarding identity
// + config. Returns nil on transport failure (daemon falls back to the manifest).
func (p *ProxyModule) Tools(ctx context.Context) []tool.Spec {
	if p.invokeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.invokeTimeout)
		defer cancel()
	}
	conn, err := p.picker.Pick(ctx, p.kind)
	if err != nil {
		return nil
	}
	req := &service.ToolsRequest{ModuleID: p.manifest.ID}
	if id, ok := tool.IdentityFromContext(ctx); ok {
		req.AppID, req.UserID, req.AgentID = id.AppID, id.UserID, id.AgentID
	}
	if cfg := pkgmodule.ModuleConfigFrom(ctx); len(cfg) > 0 {
		if b, err := json.Marshal(cfg); err == nil {
			req.Config = b
		}
	}
	// Forward a daemon-resolved listing credential (set for OAuth-gated MCP
	// servers) so the worker can CONNECT and list their tools — without it the
	// server 401s and the agent sees no tools.
	if ac, ok := pkgmodule.AuthContextFrom(ctx); ok && ac.Token != "" {
		req.AuthContext = &service.AuthContext{
			Token:        ac.Token,
			TokenType:    ac.TokenType,
			EnvTokenVar:  ac.EnvTokenVar,
			ExpiresAt:    ac.ExpiresAt,
			Provider:     ac.Provider,
			RefreshToken: ac.RefreshToken,
			Scope:        ac.Scope,
			ClientID:     ac.ClientID,
			ClientSecret: ac.ClientSecret,
		}
	}
	out := new(service.ToolsResponse)
	if err := conn.GRPC().Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodTools,
		req, out,
		grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		return nil
	}
	return out.Tools
}

// needsAuthResult builds the canonical blocking result for a missing/expired MCP
// credential. The metadata rides to the client (which drives the OAuth flow) and
// is invisible to the model; the Error text is what the model sees.
func needsAuthResult(c *service.AuthChallenge) tool.Result {
	return tool.Result{
		Success: false,
		Error:   "requires authentication for " + c.Provider,
		Metadata: map[string]any{
			"requires_oauth": true,
			"provider":       c.Provider,
			"server_id":      c.ServerID,
			"auth_url":       c.AuthURL,
			"state":          c.State,
		},
	}
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
