// Package worker hosts Digitorn modules inside a subprocess and exposes
// them over gRPC via internal/module/service. The same code path
// works for any module : the worker decides which modules to start
// from the DIGITORN_WORKER_MODULES env var, registers them in a
// local servicebus, and serves the generic ModuleService over gRPC.
//
// Used by cmd/digitorn-worker (the generic binary). Heavy modules
// (lsp, mcp, browser, ocr) live behind this pattern ; light ones
// stay in-process in the daemon.
package worker

import (
	"context"
	"encoding/json"
	"sort"
	"sync/atomic"
	"time"

	"github.com/mbathepaul/digitorn/internal/core/servicebus"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/module/service"
	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

// moduleService implements service.Service by dispatching through a
// local servicebus.Bus. Stateless on the call path : the bus owns the
// modules, this struct only times invocations and echoes RequestID.
type moduleService struct {
	bus      *servicebus.Bus
	workerID string

	// embedder/reranker, when non-nil, are injected into every Invoke
	// context so worker-hosted modules reach those daemon services via
	// the gateway.
	embedder pkgmodule.Embedder
	reranker pkgmodule.Reranker

	// invocations is a monotonic counter exposed for diagnostics.
	// Atomic so the running worker can read it lock-free from a
	// future /metrics endpoint without locking the hot path.
	invocations atomic.Uint64
}

// newModuleService binds a service.Service implementation to an
// already-running bus. The caller owns the bus lifecycle. embedder /
// reranker may be nil (no daemon services available to this worker pool).
func newModuleService(bus *servicebus.Bus, workerID string, embedder pkgmodule.Embedder, reranker pkgmodule.Reranker) *moduleService {
	return &moduleService{bus: bus, workerID: workerID, embedder: embedder, reranker: reranker}
}

// Invoke dispatches one tool call through the in-process bus and
// times it. The result is always returned via tool.Result ; gRPC
// errors are reserved for transport / framework failures so the
// daemon-side retry logic can tell them apart from module errors.
func (s *moduleService) Invoke(ctx context.Context, req *service.InvokeRequest) (*service.InvokeResponse, error) {
	s.invocations.Add(1)
	start := time.Now()

	// Re-establish the caller identity inside the worker so the module (and
	// its author middleware) see who is calling. module.Base.Invoke refines
	// it with the concrete module/tool.
	ctx = tool.WithIdentity(ctx, tool.Identity{
		AppID: req.AppID, SessionID: req.SessionID, UserID: req.UserID, AgentID: req.AgentID,
		ModuleID: req.ModuleID, ToolName: req.ToolName,
	})
	// Bridge the daemon embeddings service into the call so worker-hosted
	// modules (RAG) can embed without a local worker.Manager.
	if s.embedder != nil {
		ctx = pkgmodule.WithEmbedder(ctx, s.embedder)
	}
	if s.reranker != nil {
		ctx = pkgmodule.WithReranker(ctx, s.reranker)
	}
	// Re-inject the app's per-module config so the worker-hosted module
	// reads its app-specific configuration on this call.
	if len(req.Config) > 0 {
		var cfg map[string]any
		if json.Unmarshal(req.Config, &cfg) == nil && len(cfg) > 0 {
			ctx = pkgmodule.WithModuleConfig(ctx, cfg)
		}
	}
	// Re-inject the daemon-resolved per-user credential (MCP OAuth). Per-call,
	// never cached; the module applies it as an http header or stdio env.
	if req.AuthContext != nil {
		ctx = pkgmodule.WithAuthContext(ctx, pkgmodule.AuthContext{
			Token:        req.AuthContext.Token,
			TokenType:    req.AuthContext.TokenType,
			EnvTokenVar:  req.AuthContext.EnvTokenVar,
			ExpiresAt:    req.AuthContext.ExpiresAt,
			Provider:     req.AuthContext.Provider,
			RefreshToken: req.AuthContext.RefreshToken,
			Scope:        req.AuthContext.Scope,
			ClientID:     req.AuthContext.ClientID,
			ClientSecret: req.AuthContext.ClientSecret,
		})
	}

	res, callErr := s.bus.Call(ctx, req.ModuleID, req.ToolName, []byte(req.Params))

	// Module-level dispatch errors (unknown module, bad params, …) travel
	// inside res.Error so the daemon does not retry them as transport
	// failures. The bus usually mirrors callErr into res.Error already, but
	// never let a non-nil callErr produce a success-looking, message-less
	// result — surface it so failures are visible end to end.
	if callErr != nil {
		res.Success = false
		if res.Error == "" {
			res.Error = callErr.Error()
		}
	}

	resp := &service.InvokeResponse{
		Result:     res,
		RequestID:  req.RequestID,
		DurationMs: time.Since(start).Milliseconds(),
	}
	return resp, nil
}

// Manifests returns every module manifest the worker hosts, sorted by
// ID for deterministic output. Allows the daemon to validate at boot
// that the worker is actually serving what its config promised.
func (s *moduleService) Manifests(ctx context.Context, _ *service.ManifestsRequest) (*service.ManifestsResponse, error) {
	mods := s.bus.List()
	out := make([]domainmodule.Manifest, 0, len(mods))
	for _, m := range mods {
		out = append(out, m.Manifest())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &service.ManifestsResponse{Modules: out, WorkerID: s.workerID}, nil
}

// Tools reports a module's runtime tools, injecting identity + config into ctx
// as Invoke does. Non-LiveTooler modules fall back to their static manifest.
func (s *moduleService) Tools(ctx context.Context, req *service.ToolsRequest) (*service.ToolsResponse, error) {
	ctx = tool.WithIdentity(ctx, tool.Identity{
		AppID: req.AppID, UserID: req.UserID, AgentID: req.AgentID, ModuleID: req.ModuleID,
	})
	if len(req.Config) > 0 {
		var cfg map[string]any
		if json.Unmarshal(req.Config, &cfg) == nil && len(cfg) > 0 {
			ctx = pkgmodule.WithModuleConfig(ctx, cfg)
		}
	}
	// Per-user credential so an OAuth-gated server can be CONNECTED while listing
	// its tools (mirrors Invoke) — otherwise it 401s and the agent sees no tools.
	if req.AuthContext != nil {
		ctx = pkgmodule.WithAuthContext(ctx, pkgmodule.AuthContext{
			Token:        req.AuthContext.Token,
			TokenType:    req.AuthContext.TokenType,
			EnvTokenVar:  req.AuthContext.EnvTokenVar,
			ExpiresAt:    req.AuthContext.ExpiresAt,
			Provider:     req.AuthContext.Provider,
			RefreshToken: req.AuthContext.RefreshToken,
			Scope:        req.AuthContext.Scope,
			ClientID:     req.AuthContext.ClientID,
			ClientSecret: req.AuthContext.ClientSecret,
		})
	}
	mod, ok := s.bus.Get(req.ModuleID)
	if !ok {
		return &service.ToolsResponse{ModuleID: req.ModuleID, WorkerID: s.workerID}, nil
	}
	var specs []tool.Spec
	if lt, ok := mod.(domainmodule.LiveTooler); ok {
		specs = lt.LiveTools(ctx)
	} else {
		specs = mod.Manifest().Tools
	}
	return &service.ToolsResponse{ModuleID: req.ModuleID, Tools: specs, WorkerID: s.workerID}, nil
}

// Invocations returns the running count of Invoke calls for tests
// and diagnostics endpoints.
func (s *moduleService) Invocations() uint64 { return s.invocations.Load() }
