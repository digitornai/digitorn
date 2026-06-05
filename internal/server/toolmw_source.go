package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/embeddings"
	"github.com/mbathepaul/digitorn/internal/runtime/dispatch"
	"github.com/mbathepaul/digitorn/internal/toolmw"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// toolPipelineSource resolves the per-app tool-call middleware onion for an
// (app, module) pair from the app's tools.modules.<id>.middleware config. It
// satisfies dispatch.PipelineSource and is wired into the BusAdapter at boot.
//
// One pipeline instance is built per (app, module) and cached : the middleware
// hold state (circuit breaker health, dedup / semantic caches, budget window)
// that must survive across calls and sessions. Per-session isolation lives
// INSIDE each stateful middleware (keyed by session id), so a single shared
// instance is both correct and the only way the app-global layers
// (circuit_breaker, budget) can do their job. A nil result is cached for pairs
// with no middleware so the hot path stays allocation-free.
// appGetter is the narrow slice of appmgr.Manager this source needs : resolve
// an app's runtime view. *appmgr.gormManager (the production Manager) satisfies
// it ; tests provide a one-line fake.
type appGetter interface {
	Get(ctx context.Context, appID string) (*appmgr.RuntimeApp, error)
}

type toolPipelineSource struct {
	apps   appGetter
	deps   toolmw.Deps
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]dispatch.ToolPipeline
}

func newToolPipelineSource(apps appGetter, emb *embeddings.Client, resolver toolmw.ToolResolver, logger *slog.Logger) *toolPipelineSource {
	if logger == nil {
		logger = slog.Default()
	}
	deps := toolmw.Deps{Logger: logger, ToolResolver: resolver}
	if emb != nil {
		deps.Embedder = embedderAdapter{c: emb}
	}
	return &toolPipelineSource{
		apps:   apps,
		deps:   deps,
		logger: logger,
		cache:  map[string]dispatch.ToolPipeline{},
	}
}

// newToolResolver builds an auto_heal ToolResolver over the module registry :
// on a failed (module, tool) call it proposes the module's OTHER tools first,
// then same-named tools hosted by other modules. Both are precise, no fuzzy
// matching — exactly the alternatives an agent can usefully retry. nil registry
// yields a nil resolver (auto_heal stays inert).
func newToolResolver(reg *module.Registry) toolmw.ToolResolver {
	if reg == nil {
		return nil
	}
	return func(moduleID, toolName string) []toolmw.ToolSuggestion {
		var same, cross []toolmw.ToolSuggestion
		for _, mf := range reg.Manifests() {
			for _, spec := range mf.Tools {
				switch {
				case mf.ID == moduleID && spec.Name != toolName:
					same = append(same, toolmw.ToolSuggestion{
						ModuleID: mf.ID, ToolName: spec.Name, Description: spec.Description,
					})
				case mf.ID != moduleID && spec.Name == toolName:
					cross = append(cross, toolmw.ToolSuggestion{
						ModuleID: mf.ID, ToolName: spec.Name, Description: spec.Description,
					})
				}
			}
		}
		return append(same, cross...)
	}
}

func (s *toolPipelineSource) PipelineFor(appID, moduleID string) dispatch.ToolPipeline {
	key := appID + "\x00" + moduleID
	s.mu.RLock()
	p, ok := s.cache[key]
	s.mu.RUnlock()
	if ok {
		return p
	}
	return s.resolve(key, appID, moduleID)
}

func (s *toolPipelineSource) resolve(key, appID, moduleID string) dispatch.ToolPipeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.cache[key]; ok {
		return p
	}

	var result dispatch.ToolPipeline
	if ra, err := s.apps.Get(context.Background(), appID); err == nil &&
		ra != nil && ra.Definition != nil && ra.Definition.Tools != nil {
		if mb, ok := ra.Definition.Tools.Modules[moduleID]; ok && len(mb.Middleware) > 0 {
			if pipe := toolmw.Build(mb.Middleware, s.deps, s.logger); pipe != nil {
				result = pipe
			}
		}
	}
	s.cache[key] = result
	return result
}

// embedderAdapter bridges the daemon embeddings client (batch, []Vector) to the
// toolmw.Embedder seam (single text, []float32) used by semantic_cache.
type embedderAdapter struct{ c *embeddings.Client }

func (e embedderAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.c.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		return nil, err
	}
	return []float32(vecs[0]), nil
}

var _ dispatch.PipelineSource = (*toolPipelineSource)(nil)
