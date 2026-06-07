package server

import (
	"context"
	"strings"
	"sync"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// mcpCatalog materializes worker-hosted MCP tools into native AvailableAction /
// tool.Spec shapes. bySpec is keyed by FQN (LookupToolSpec has no appID; an MCP
// tool's spec is the server's, identical across apps).
type mcpCatalog struct {
	registry *module.Registry
	apps     appmgr.Manager

	mu     sync.RWMutex
	byApp  map[string][]policy.AvailableAction
	bySpec map[string]*tool.Spec
}

func newMCPCatalog(reg *module.Registry, apps appmgr.Manager) *mcpCatalog {
	return &mcpCatalog{
		registry: reg, apps: apps,
		byApp:  map[string][]policy.AvailableAction{},
		bySpec: map[string]*tool.Spec{},
	}
}

func (mc *mcpCatalog) forApp(appID string) []policy.AvailableAction {
	if mc == nil || mc.registry == nil || appID == "" {
		return nil
	}
	mc.mu.RLock()
	cached, ok := mc.byApp[appID]
	mc.mu.RUnlock()
	if ok {
		return cached
	}
	actions := mc.fetch(appID)
	mc.mu.Lock()
	mc.byApp[appID] = actions
	for i := range actions {
		if actions[i].Spec != nil {
			mc.bySpec[actions[i].Spec.Name] = actions[i].Spec
		}
	}
	mc.mu.Unlock()
	return actions
}

func (mc *mcpCatalog) lookupSpec(moduleID, action string) *tool.Spec {
	if mc == nil {
		return nil
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.bySpec[moduleID+"."+action]
}

func (mc *mcpCatalog) invalidate(appID string) {
	if mc == nil {
		return
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if appID == "" {
		mc.byApp = map[string][]policy.AvailableAction{}
		mc.bySpec = map[string]*tool.Spec{}
		return
	}
	delete(mc.byApp, appID)
}

func (mc *mcpCatalog) fetch(appID string) []policy.AvailableAction {
	cfg := mc.appMCPConfig(appID)
	if cfg == nil {
		return nil // app does not declare the mcp module
	}
	mod, err := mc.registry.Get("mcp")
	if err != nil || mod == nil {
		return nil
	}
	ctx := module.WithModuleConfig(context.Background(), cfg)
	ctx = tool.WithIdentity(ctx, tool.Identity{AppID: appID, ModuleID: "mcp"})

	specs := mcpLiveSpecs(ctx, mod)
	out := make([]policy.AvailableAction, 0, len(specs))
	for i := range specs {
		modID, action := toolname.SplitFQN(toolname.Canonicalize(specs[i].Name))
		if modID == "" || action == "" {
			continue
		}
		fqn := specs[i]
		fqn.Name = modID + "." + action
		out = append(out, policy.AvailableAction{Module: modID, Action: action, Spec: &fqn})
	}
	return out
}

func (mc *mcpCatalog) appMCPConfig(appID string) map[string]any {
	if mc.apps == nil {
		return nil
	}
	rt, err := mc.apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Tools == nil {
		return nil
	}
	blk, ok := rt.Definition.Tools.Modules["mcp"]
	if !ok {
		return nil
	}
	return blk.Config
}

func mcpLiveSpecs(ctx context.Context, mod domainmodule.Module) []tool.Spec {
	if pm, ok := mod.(interface {
		Tools(context.Context) []tool.Spec
	}); ok {
		return pm.Tools(ctx)
	}
	if lt, ok := mod.(domainmodule.LiveTooler); ok {
		return lt.LiveTools(ctx)
	}
	return nil
}

func isMCPModule(moduleID string) bool {
	return moduleID == "mcp" || strings.HasPrefix(moduleID, "mcp_")
}
