package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// piecesCatalog materializes pieces bridge tools into native AvailableAction /
// tool.Spec shapes, following the same pattern as mcpCatalog.
type piecesCatalog struct {
	registry *module.Registry
	apps     appmgr.Manager

	mu     sync.RWMutex
	byApp  map[string][]policy.AvailableAction
	bySpec map[string]*tool.Spec
}

func newPiecesCatalog(reg *module.Registry, apps appmgr.Manager) *piecesCatalog {
	return &piecesCatalog{
		registry: reg, apps: apps,
		byApp:  map[string][]policy.AvailableAction{},
		bySpec: map[string]*tool.Spec{},
	}
}

func (pc *piecesCatalog) forApp(appID string) []policy.AvailableAction {
	if pc == nil || pc.registry == nil || appID == "" {
		slog.Debug("pieces_catalog: forApp early return", "pc_nil", pc == nil, "registry_nil", pc != nil && pc.registry == nil, "appID", appID)
		return nil
	}
	pc.mu.RLock()
	cached, ok := pc.byApp[appID]
	pc.mu.RUnlock()
	if ok {
		slog.Debug("pieces_catalog: forApp cache hit", "app_id", appID, "count", len(cached))
		return cached
	}
	actions := pc.fetch(appID)
	pc.mu.Lock()
	pc.byApp[appID] = actions
	for i := range actions {
		if actions[i].Spec != nil {
			pc.bySpec[actions[i].Spec.Name] = actions[i].Spec
		}
	}
	pc.mu.Unlock()
	slog.Debug("pieces_catalog: forApp fetch done", "app_id", appID, "count", len(actions))
	return actions
}

func (pc *piecesCatalog) fetch(appID string) []policy.AvailableAction {
	if !pc.declaredPieces(appID) {
		slog.Debug("pieces_catalog: app does not declare pieces module", "app_id", appID)
		return nil
	}
	mod, err := pc.registry.Get("pieces")
	if err != nil || mod == nil {
		slog.Debug("pieces_catalog: pieces module not found in registry", "app_id", appID, "err", err)
		return nil
	}
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: appID, ModuleID: "pieces"})
	specs := piecesLiveSpecs(ctx, mod)
	if len(specs) == 0 {
		slog.Debug("pieces_catalog: no live tools from bridge", "app_id", appID)
		return nil
	}
	slog.Debug("pieces_catalog: fetched tools from bridge", "app_id", appID, "count", len(specs))
	out := make([]policy.AvailableAction, 0, len(specs))
	for i := range specs {
		// Pieces tools are named "ap_{piece}__{action}".
		// Canonicalize converts to "ap_{piece}.{action}" → SplitFQN gives module="ap_{piece}", action="action".
		// We register with module="ap_{piece}" so the index FQN is "ap_{piece}.action"
		// (matches what the LLM sees). Gate1a resolves "ap_*" via the "pieces" umbrella.
		canonical := toolname.Canonicalize(specs[i].Name)
		modID, action := toolname.SplitFQN(canonical)
		if modID == "" || action == "" {
			continue
		}
		fqnSpec := specs[i]
		fqnSpec.Name = canonical // "ap_{piece}.action"
		out = append(out, policy.AvailableAction{
			Module: modID,  // "ap_{piece}" — gate1a resolves via "pieces" umbrella
			Action: action, // "action" — bare action name
			Spec:   &fqnSpec,
		})
	}
	return out
}

func (pc *piecesCatalog) lookupSpec(moduleID, action string) *tool.Spec {
	if pc == nil {
		return nil
	}
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	// Try canonical FQN form ("ap_{piece}.action")
	if s := pc.bySpec[moduleID+"."+action]; s != nil {
		return s
	}
	return nil
}

func (pc *piecesCatalog) invalidate(appID string) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if appID == "" {
		pc.byApp = map[string][]policy.AvailableAction{}
		pc.bySpec = map[string]*tool.Spec{}
		return
	}
	delete(pc.byApp, appID)
}

func (pc *piecesCatalog) declaredPieces(appID string) bool {
	if pc.apps == nil {
		slog.Debug("pieces_catalog: declaredPieces apps nil")
		return false
	}
	rt, err := pc.apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Tools == nil {
		slog.Debug("pieces_catalog: declaredPieces app lookup failed", "err", err, "rt_nil", rt == nil)
		return false
	}
	_, ok := rt.Definition.Tools.Modules["pieces"]
	slog.Debug("pieces_catalog: declaredPieces result", "app_id", appID, "has_pieces", ok, "modules", getModuleKeys(rt.Definition.Tools.Modules))
	return ok
}

func getModuleKeys(mods map[string]schema.ModuleBlock) []string {
	if mods == nil {
		return nil
	}
	keys := make([]string, 0, len(mods))
	for k := range mods {
		keys = append(keys, k)
	}
	return keys
}

func piecesLiveSpecs(ctx context.Context, mod domainmodule.Module) []tool.Spec {
	if lt, ok := mod.(domainmodule.LiveTooler); ok {
		return lt.LiveTools(ctx)
	}
	return nil
}
