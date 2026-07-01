package server

import (
	"context"
	"strings"

	"github.com/digitornai/digitorn/internal/appmgr"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/pkg/module"
)

// registryActions implements wiring.AvailableActions over the
// daemon's module registry + app manager. For each app, it returns
// the AvailableAction tuples corresponding to the modules the app
// has DECLARED in its tools.modules block, intersected with what
// is actually loaded in the registry.
//
// Why this intersection :
//
//   - The app declares which modules it intends to use in YAML
//     (tools.modules). That declaration is the contractual surface
//     of the app — only those modules can be reached.
//   - The registry knows which modules are actually loaded (some
//     might be missing because the daemon failed to start them,
//     or the binary doesn't include them on this platform).
//   - Tools from the intersection are eligible. SG-3 + agent.Modules
//     subset then filter further per agent.
//
// Anti-leak invariant : a RESOLVED app sees ONLY the modules it
// declared. An app with no tools.modules block (or an empty one)
// gets the EMPTY set — never the registry universe. The permissive
// "expose everything" path is reserved for the UNRESOLVED case
// (no app-manager wired, empty appID, unknown app) which only the
// unit tests and isolated dev harnesses ever hit.
//
// Concurrency : safe to call concurrently. The registry and
// app-manager are concurrency-safe by themselves ; this struct
// only reads.
type registryActions struct {
	Registry *module.Registry
	Apps     appmgr.Manager
	MCP      *mcpCatalog
	Pieces   *piecesCatalog
}

// ForApp implements wiring.AvailableActions. ctx-free contract
// (the wiring layer doesn't propagate a context), so we use
// context.Background() for the app lookup — cheap, no I/O on the
// hot path (appmgr.Get reads the in-memory snapshot).
func (a registryActions) ForApp(appID string) []policy.AvailableAction {
	if a.Registry == nil {
		return nil
	}
	manifests := a.Registry.Manifests()
	if len(manifests) == 0 {
		return nil
	}

	declared := a.declaredModulesFor(appID)

	out := make([]policy.AvailableAction, 0, 32)
	for _, mf := range manifests {
		if declared != nil {
			if _, ok := declared[mf.ID]; !ok {
				continue // app didn't declare this module
			}
		}
		for _, spec := range mf.Tools {
			// Build a copy of the spec with the FQN as Name. The
			// manifest stores the bare action name ("read") ; the
			// policy / index / dispatcher contract uses FQN
			// ("filesystem.read"). Copying keeps the manifest
			// pristine for other consumers.
			fqnSpec := spec
			fqnSpec.Name = mf.ID + "." + spec.Name
			out = append(out, policy.AvailableAction{
				Module: mf.ID,
				Action: spec.Name,
				Spec:   &fqnSpec,
			})
		}
	}
	// MCP virtual tools. Apply the per-app allowed_servers allow-list HERE so a
	// disallowed server's tools never enter the universe → never reach the index
	// or the LLM (the same build-time filtering gate 1a gives native modules).
	// Gate 1c re-checks at dispatch, so this is defense-in-depth, not the only
	// guard. nil allow-set = no restriction.
	mcpActions := a.MCP.forApp(appID) // forApp self-gates on the mcp config block
	if allowed := a.mcpAllowedServers(appID); allowed != nil {
		mcpActions = filterMCPByServer(mcpActions, allowed)
	}
	out = append(out, mcpActions...)
	// Pieces virtual tools — same pattern as MCP: fetch LiveTools from the
	// pieces bridge and inject them into the universe so the tool index sees them.
	piecesActions := a.Pieces.forApp(appID)
	out = append(out, piecesActions...)
	return out
}

// mcpAllowedServers reads tools.modules.mcp.constraints.allowed_servers as a
// set. nil = the constraint is absent (no restriction). A present-but-empty list
// yields a non-nil empty set (no MCP server allowed).
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

// filterMCPByServer keeps only the MCP actions whose mcp_<server> module is in
// the allowed set. Returns a NEW slice (never mutates the catalog's cache).
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

// declaredModulesFor returns the set of modules an app explicitly enabled
// via tools.modules.<id> — the documented activation gate. The return
// distinguishes two cases that ForApp treats very differently :
//
//   - nil : the app could NOT be resolved (no app-manager wired, empty
//     appID, unknown app). ForApp applies NO filter — permissive, used
//     by unit tests that run without an app manager.
//   - non-nil (possibly EMPTY) : the app WAS resolved. ForApp restricts
//     the universe to exactly these modules. An app that declares no
//     modules therefore gets ZERO domain modules — never the whole
//     registry. This is the anti-leak invariant : an agent only ever
//     sees a module the app explicitly enabled.
func (a registryActions) declaredModulesFor(appID string) map[string]struct{} {
	if a.Apps == nil || appID == "" {
		return nil // unresolved → no filter (test/dev path)
	}
	rt, err := a.Apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil {
		return nil // unknown app → no filter
	}
	// App resolved : build the exact declared set (possibly empty → strict).
	set := make(map[string]struct{})
	if rt.Definition.Tools != nil {
		for id := range rt.Definition.Tools.Modules {
			set[id] = struct{}{}
		}
	}
	return set
}

// Compile-time guard : a registryActions must satisfy the
// runtime context_builder action source contract. The interface
// lives in wiring/, but pulling it as a constraint here would
// create an import cycle, so we just rely on the wiring code
// asking for the same shape (ForApp(string) []AvailableAction).
var _ interface {
	ForApp(string) []policy.AvailableAction
} = registryActions{}

// registryContributors implements wiring.PromptContributors over the module
// registry. For each module the agent is AUTHORIZED for (passed in by the
// wiring layer as the per-agent index categories), it asks the live module
// instance — if it implements domainmodule.PromptContributor — for its
// system-prompt sections and dynamic per-tool prompts. This is the faithful
// port of the reference daemon's get_prompt_sections() /
// get_dynamic_tool_prompts() : the framework gathers automatically, the
// module writes ZERO assembler code.
//
// Authorization is enforced by the caller (only authorized module IDs are
// passed), so an unauthorized module is never even asked — the anti-leak
// invariant holds at the source.
type registryContributors struct {
	Registry *module.Registry
}

func (c registryContributors) Gather(scope domainmodule.PromptScope, authorizedModules []string) ([]domainmodule.PromptSection, map[string]string) {
	if c.Registry == nil || len(authorizedModules) == 0 {
		return nil, nil
	}
	var sections []domainmodule.PromptSection
	var dynamic map[string]string
	for _, id := range authorizedModules {
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
	return sections, dynamic
}

// registryToolSpecs implements runtime.ToolSpecLookup over the module
// registry. It resolves a (module, action) pair to the action's
// tool.Spec so the runtime gates (SG-4) can read RiskLevel and
// required Permissions, and so approval rows carry the real risk
// level. Returns nil for unknown pairs ; gates 2/3 fail closed on a
// nil spec, which is the documented stance for an action the daemon
// can't vouch for. Meta-tools and system modules never reach here —
// RunGates short-circuits them before any spec lookup.
type registryToolSpecs struct {
	Registry *module.Registry
	MCP      *mcpCatalog
	Pieces   *piecesCatalog
}

func (r registryToolSpecs) LookupToolSpec(moduleID, action string) *tool.Spec {
	// mcp_<server> tools are runtime-discovered: resolve via the live catalog so
	// the gates read a real spec instead of failing closed.
	if r.MCP != nil && isMCPModule(moduleID) {
		if s := r.MCP.lookupSpec(moduleID, action); s != nil {
			return s
		}
	}
	// ap_<piece> tools are runtime-discovered from the pieces bridge.
	// The LLM sees them as "ap_{piece}.{action}" and they're registered
	// under module "ap_{piece}" with the canonical FQN.
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

// Static fallback for unit tests : a fixed universe of actions.
// Used when the daemon hasn't fully booted (e.g. unit tests
// that call buildEngine in isolation).
type staticActions struct {
	all []policy.AvailableAction
}

func (s staticActions) ForApp(string) []policy.AvailableAction { return s.all }

// Empty universe (the daemon's old "no modules wired" stub).
// Kept as a typed value for backwards-compatible tests and for
// dev mode when the registry is intentionally empty.
var _ = staticActions{} // silence unused linter
var _ tool.Spec = tool.Spec{}
