package compiler

import (
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// moduleAliases maps a legacy module id to its current replacement. The old
// `workspace` file module (Write/Read/Edit/Glob/Grep/Delete) is now served by
// `filesystem`, so an app written against the old daemon keeps working unchanged.
//
// The new internal git-tracking module is ALSO named `workspace`, but it is
// daemon-wired (Internal tools, never declared by an app), so this app-level
// rewrite never reaches it: an app that declares `workspace` always means the
// legacy file module, which we redirect to `filesystem`.
var moduleAliases = map[string]string{
	"workspace": "filesystem",
}

// aliasConfigKeep lists the config keys a TARGET module understands. When a
// legacy module is aliased, any config key not in this set is dropped — the old
// `workspace` module carried preview/UI hints (render_mode, entry_file,
// sync_to_disk, auto_approve, lint, title, …) that the `filesystem` module has
// no schema for; the new preview system reads those elsewhere, so they must not
// fail validation as "unknown config field". A target absent here keeps all
// config (no filtering).
var aliasConfigKeep = map[string]map[string]bool{
	"filesystem": {"workspace": true, "max_file_bytes": true},
}

func aliasModuleID(id string) string {
	if to, ok := moduleAliases[id]; ok {
		return to
	}
	return id
}

// keepAliasedConfig drops config keys the alias target does not understand.
func keepAliasedConfig(target string, cfg map[string]any) map[string]any {
	allow, ok := aliasConfigKeep[target]
	if !ok || cfg == nil {
		return cfg
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if allow[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeModuleAliases rewrites every legacy module reference in the compiled
// app definition to its current id BEFORE validation runs, so validation, the
// agent toolset and the dispatcher only ever see the real module. Idempotent.
func normalizeModuleAliases(def *schema.AppDefinition) {
	if def == nil {
		return
	}
	if def.Tools != nil {
		aliasModulesMap(def.Tools.Modules)
		aliasCapabilities(def.Tools.Capabilities)
	}
	// Defensive: handle the legacy top-level aliases too, in case they survive
	// the fold (foldLegacyAliases runs first, but this keeps the pass order-free).
	aliasModulesMap(def.ModulesTop)
	aliasCapabilities(def.CapabilitiesTop)
	if def.Runtime != nil {
		aliasStringList(def.Runtime.DirectModules)
	}
	for i := range def.Agents {
		def.Agents[i].Modules = aliasAgentModules(def.Agents[i].Modules)
		aliasCapabilities(def.Agents[i].Capabilities.Config)
	}
}

// aliasModulesMap renames an aliased module key to its target, merging into an
// existing explicit target block (the explicit block wins — the legacy alias's
// config is dropped rather than silently overriding it).
func aliasModulesMap(m map[string]schema.ModuleBlock) {
	if m == nil {
		return
	}
	for old, to := range moduleAliases {
		blk, ok := m[old]
		if !ok {
			continue
		}
		blk.Config = keepAliasedConfig(to, blk.Config)
		if _, exists := m[to]; !exists {
			m[to] = blk
		}
		delete(m, old)
	}
}

func aliasCapabilities(c *schema.CapabilitiesConfig) {
	if c == nil {
		return
	}
	aliasGrants(c.Grant)
	aliasGrants(c.Approve)
	aliasGrants(c.Deny)
	aliasGrants(c.HiddenActions)
	aliasStringList(c.HiddenModules)
	if c.RateLimits != nil {
		for k, v := range c.RateLimits {
			nk := aliasFQN(k)
			if nk == k {
				continue
			}
			delete(c.RateLimits, k)
			if _, exists := c.RateLimits[nk]; !exists {
				c.RateLimits[nk] = v
			}
		}
	}
}

func aliasGrants(gs []schema.CapabilityGrant) {
	for i := range gs {
		gs[i].Module = aliasModuleID(gs[i].Module)
	}
}

func aliasStringList(xs []string) {
	for i := range xs {
		xs[i] = aliasModuleID(xs[i])
	}
}

// aliasAgentModules rewrites each module ref id and dedups: if aliasing makes an
// agent list the same target twice (it had both `workspace` and `filesystem`),
// the refs merge. An empty tool list means "all tools" and dominates a subset.
func aliasAgentModules(refs schema.AgentModules) schema.AgentModules {
	if len(refs) == 0 {
		return refs
	}
	out := make(schema.AgentModules, 0, len(refs))
	idx := make(map[string]int, len(refs))
	for _, r := range refs {
		r.ID = aliasModuleID(r.ID)
		if at, ok := idx[r.ID]; ok {
			if len(out[at].Tools) == 0 || len(r.Tools) == 0 {
				out[at].Tools = nil // "all tools" wins over any subset
			} else {
				out[at].Tools = unionStrings(out[at].Tools, r.Tools)
			}
			continue
		}
		idx[r.ID] = len(out)
		out = append(out, r)
	}
	return out
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			a = append(a, s)
		}
	}
	return a
}

// aliasFQN rewrites the module part of a "module.action" key, leaving the action.
func aliasFQN(fqn string) string {
	i := strings.IndexByte(fqn, '.')
	if i < 0 {
		return aliasModuleID(fqn)
	}
	return aliasModuleID(fqn[:i]) + fqn[i:]
}
