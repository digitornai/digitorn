package compiler

import (
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

var moduleAliases = map[string]string{
	"workspace": "filesystem",
}

var aliasConfigKeep = map[string]map[string]bool{
	"filesystem": {"workspace": true, "max_file_bytes": true},
}

func aliasModuleID(id string) string {
	if to, ok := moduleAliases[id]; ok {
		return to
	}
	return id
}

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

func normalizeModuleAliases(def *schema.AppDefinition) {
	if def == nil {
		return
	}
	if def.Tools != nil {
		aliasModulesMap(def.Tools.Modules)
		aliasCapabilities(def.Tools.Capabilities)
	}
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
				out[at].Tools = nil
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

func aliasFQN(fqn string) string {
	i := strings.IndexByte(fqn, '.')
	if i < 0 {
		return aliasModuleID(fqn)
	}
	return aliasModuleID(fqn[:i]) + fqn[i:]
}
