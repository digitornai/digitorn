package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/digitornai/digitorn/internal/appmgr"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/pkg/module"
)

type registryActions struct {
	Registry *module.Registry
	Apps     appmgr.Manager
	MCP      *mcpCatalog
	Pieces   *piecesCatalog
}

func (a registryActions) ForApp(appID string) []policy.AvailableAction {
	if a.Registry == nil {
		return nil
	}
	manifests := a.Registry.Manifests()
	if len(manifests) == 0 {
		return nil
	}

	declared := a.declaredModulesFor(appID)
	discovery := a.discoveryModulesFor(appID)

	out := make([]policy.AvailableAction, 0, 32)
	for _, mf := range manifests {
		if declared != nil {
			if _, ok := declared[mf.ID]; !ok {
				continue
			}
		}
		for _, spec := range mf.Tools {
			fqnSpec := spec
			fqnSpec.Name = mf.ID + "." + spec.Name
			_, discoverable := discovery[mf.ID]
			out = append(out, policy.AvailableAction{
				Module:        mf.ID,
				Action:        spec.Name,
				Spec:          &fqnSpec,
				DiscoveryOnly: discoverable,
			})
		}
	}
	mcpActions := a.MCP.forApp(appID)
	if allowed := a.mcpAllowedServers(appID); allowed != nil {
		mcpActions = filterMCPByServer(mcpActions, allowed)
	}
	out = append(out, mcpActions...)
	piecesActions := a.Pieces.forApp(appID)
	out = append(out, piecesActions...)
	return out
}

func (a registryActions) mcpAllowedServers(appID string) map[string]struct{} {
	if a.Apps == nil || appID == "" {
		return nil
	}
	rt, err := a.Apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Tools == nil {
		return nil
	}
	mb, ok := rt.Definition.Tools.Modules["mcp"]
	if !ok || mb.Constraints == nil {
		return nil
	}
	raw, ok := mb.Constraints["allowed_servers"]
	if !ok {
		return nil
	}
	set := map[string]struct{}{}
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			set[s] = struct{}{}
		}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				set[s] = struct{}{}
			}
		}
	}
	return set
}

func filterMCPByServer(actions []policy.AvailableAction, allowed map[string]struct{}) []policy.AvailableAction {
	out := make([]policy.AvailableAction, 0, len(actions))
	for _, act := range actions {
		server := strings.TrimPrefix(act.Module, "mcp_")
		if _, ok := allowed[server]; ok {
			out = append(out, act)
		}
	}
	return out
}

func (a registryActions) declaredModulesFor(appID string) map[string]struct{} {
	if a.Apps == nil || appID == "" {
		return nil
	}
	rt, err := a.Apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil {
		return nil
	}
	set := make(map[string]struct{})
	if rt.Definition.Tools != nil {
		for id := range rt.Definition.Tools.Modules {
			set[id] = struct{}{}
		}
	}
	return set
}

// discoveryModulesFor returns the modules an app granted with a wildcard.
//
// A wildcard grant means "this app may use everything here", which for a large
// module is a statement about REACH, not about what belongs in the prompt.
// Injecting every schema would spend the token budget on tools the agent will
// not use this turn, so those modules become discovery-only: the agent finds
// them with search_tools / get_tool when a task calls for them.
//
// The pieces catalog already worked this way; keeping the rule there made it a
// quirk of one module rather than a property of the platform. An app can now
// hand the agent a broad capability — the web, a big connector set — without
// paying for it on every turn.
func (a registryActions) discoveryModulesFor(appID string) map[string]struct{} {
	if a.Apps == nil || appID == "" {
		return nil
	}
	rt, err := a.Apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Tools == nil {
		return nil
	}
	caps := rt.Definition.Tools.Capabilities
	if caps == nil {
		return nil
	}
	set := make(map[string]struct{})
	for _, g := range caps.Grant {
		if g.Module == "" || g.Module == "pieces" {
			continue // pieces resolves its own catalog, with live tools
		}
		for _, t := range g.EffectiveTools() {
			if t == "*" {
				set[g.Module] = struct{}{}
				break
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

var _ interface {
	ForApp(string) []policy.AvailableAction
} = registryActions{}

type registryContributors struct {
	Registry *module.Registry
	Pieces   *piecesCatalog
}

func (c registryContributors) Gather(scope domainmodule.PromptScope, authorizedModules []string) ([]domainmodule.PromptSection, map[string]string) {
	if c.Registry == nil || len(authorizedModules) == 0 {
		return nil, nil
	}
	var sections []domainmodule.PromptSection
	var dynamic map[string]string
	piecesAuthorized := false
	for _, id := range authorizedModules {
		if id == "pieces" {
			piecesAuthorized = true
		}
		m, err := c.Registry.Get(id)
		if err != nil || m == nil {
			continue
		}
		pc, ok := m.(domainmodule.PromptContributor)
		if !ok {
			continue
		}
		if secs := pc.PromptSections(scope); len(secs) > 0 {
			sections = append(sections, secs...)
		}
		for fqn, p := range pc.DynamicToolPrompts(scope) {
			if dynamic == nil {
				dynamic = make(map[string]string)
			}
			dynamic[fqn] = p
		}
	}
	if piecesAuthorized && c.Pieces != nil && scope.AppID != "" {
		if sec := piecesConnectorSection(c.Pieces.forApp(scope.AppID)); sec != nil {
			sections = append(sections, *sec)
		}
	}
	return sections, dynamic
}

const piecesSectionMaxConnectors = 40

func piecesConnectorSection(actions []policy.AvailableAction) *domainmodule.PromptSection {
	if len(actions) == 0 {
		return nil
	}
	discoveryOnly := actions[0].DiscoveryOnly
	order := make([]string, 0, 8)
	byConn := map[string][]string{}
	for _, a := range actions {
		conn := strings.TrimPrefix(a.Module, "ap_")
		if _, seen := byConn[conn]; !seen {
			order = append(order, conn)
		}
		if len(byConn[conn]) < 6 {
			byConn[conn] = append(byConn[conn], a.Action)
		}
	}
	sort.Strings(order)
	total := len(order)

	var b strings.Builder
	if discoveryOnly && total > piecesSectionMaxConnectors {
		fmt.Fprintf(&b, "This app can reach %d connectors on the user's behalf. Authentication is handled automatically — never ask the user for an API key, token, or login. The list below is a sample; use search_tools to find any connector by name or capability.\n\n", total)
	} else {
		b.WriteString("These connectors are wired to this app and ready to use on the user's behalf. Authentication is handled automatically — never ask the user for an API key, token, or login.\n\n")
	}

	shown := order
	if total > piecesSectionMaxConnectors {
		shown = order[:piecesSectionMaxConnectors]
	}
	for _, conn := range shown {
		examples := byConn[conn]
		b.WriteString("- ")
		b.WriteString(conn)
		b.WriteString(" (ap_")
		b.WriteString(conn)
		b.WriteString("__*)")
		if len(examples) > 0 {
			b.WriteString(" — e.g. ")
			b.WriteString(strings.Join(examples, ", "))
		}
		b.WriteString("\n")
	}
	if total > piecesSectionMaxConnectors {
		fmt.Fprintf(&b, "- …and %d more — use search_tools to find them.\n", total-piecesSectionMaxConnectors)
	}
	b.WriteString("\nTo act on a connector, run search_tools with the connector name to find its exact actions, inspect one with get_tool, then call ap_<connector>__<action>. If a call fails with an auth error, tell the user to connect that connector in Settings → Connectors.")

	return &domainmodule.PromptSection{
		Title:    "Connected connectors",
		Content:  b.String(),
		Priority: 60,
	}
}

type registryToolSpecs struct {
	Registry *module.Registry
	MCP      *mcpCatalog
	Pieces   *piecesCatalog
}

func (r registryToolSpecs) LookupToolSpec(moduleID, action string) *tool.Spec {
	if r.MCP != nil && isMCPModule(moduleID) {
		if s := r.MCP.lookupSpec(moduleID, action); s != nil {
			return s
		}
	}
	if r.Pieces != nil && strings.HasPrefix(moduleID, "ap_") {
		if s := r.Pieces.lookupSpec(moduleID, action); s != nil {
			return s
		}
	}
	if r.Registry == nil {
		return nil
	}
	mf, ok := r.Registry.Manifest(moduleID)
	if !ok {
		return nil
	}
	for i := range mf.Tools {
		if mf.Tools[i].Name == action {
			spec := mf.Tools[i]
			return &spec
		}
	}
	return nil
}

type staticActions struct {
	all []policy.AvailableAction
}

func (s staticActions) ForApp(string) []policy.AvailableAction { return s.all }

var _ = staticActions{}
var _ tool.Spec = tool.Spec{}
