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
	"sort"
	"sync/atomic"
	"time"

	"github.com/mbathepaul/digitorn/internal/core/servicebus"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/module/service"
)

// moduleService implements service.Service by dispatching through a
// local servicebus.Bus. Stateless on the call path : the bus owns the
// modules, this struct only times invocations and echoes RequestID.
type moduleService struct {
	bus      *servicebus.Bus
	workerID string

	// invocations is a monotonic counter exposed for diagnostics.
	// Atomic so the running worker can read it lock-free from a
	// future /metrics endpoint without locking the hot path.
	invocations atomic.Uint64
}

// newModuleService binds a service.Service implementation to an
// already-running bus. The caller owns the bus lifecycle.
func newModuleService(bus *servicebus.Bus, workerID string) *moduleService {
	return &moduleService{bus: bus, workerID: workerID}
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

// Invocations returns the running count of Invoke calls for tests
// and diagnostics endpoints.
func (s *moduleService) Invocations() uint64 { return s.invocations.Load() }
