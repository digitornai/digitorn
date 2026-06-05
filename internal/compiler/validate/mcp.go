package validate

import (
	"fmt"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/compiler/suggest"
)

func CheckMCPRefs(file string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	servers := declaredMCPServers(def)
	for i, h := range def.RuntimeHooksOrNil() {
		checkHookForMCPRef(h, servers, fmt.Sprintf("runtime.hooks.%d", i), bag)
	}
	for ai, a := range def.Agents {
		for hi, h := range a.Hooks {
			checkHookForMCPRef(h, servers, fmt.Sprintf("agents.%d.hooks.%d", ai, hi), bag)
		}
	}
	if def.Security != nil && def.Security.CredentialsSchema != nil {
		for i, p := range def.Security.CredentialsSchema.Providers {
			if p.Type != schema.CredTypeMCPServer || p.Name == "" {
				continue
			}
			if _, ok := servers[p.Name]; ok || len(servers) == 0 {
				continue
			}
			d := diagnostic.Errorf(diagnostic.CodeUnknownMCPServer, posUnknown,
				"security.credentials_schema.providers.%d: MCP server %q is not declared in tools.modules.mcp.config.servers",
				i, p.Name)
			if s, _ := suggest.Closest(p.Name, mcpKeys(servers), 2); s != "" {
				d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
			}
			bag.Add(d)
		}
	}
}

func declaredMCPServers(def *schema.AppDefinition) map[string]struct{} {
	out := map[string]struct{}{}
	if def.Tools == nil {
		return out
	}
	mcp, ok := def.Tools.Modules["mcp"]
	if !ok {
		return out
	}
	cfg, ok := mcp.Config["servers"]
	if !ok {
		return out
	}
	switch v := cfg.(type) {
	case map[string]any:
		for k := range v {
			out[k] = struct{}{}
		}
	case map[any]any:
		for k := range v {
			if s, ok := k.(string); ok {
				out[s] = struct{}{}
			}
		}
	case []any:
		for _, it := range v {
			if m, ok := it.(map[string]any); ok {
				if name, ok := m["id"].(string); ok {
					out[name] = struct{}{}
				} else if name, ok := m["name"].(string); ok {
					out[name] = struct{}{}
				}
			}
		}
	}
	return out
}

func checkHookForMCPRef(h schema.Hook, servers map[string]struct{}, path string, bag *diagnostic.Bag) {
	if h.Action.Type != schema.ActionModuleAction {
		return
	}
	mod, _ := h.Action.Params["module"].(string)
	if mod != "mcp" {
		return
	}
	params, _ := h.Action.Params["params"].(map[string]any)
	server, _ := params["server"].(string)
	if server == "" {
		return
	}
	if _, ok := servers[server]; ok {
		return
	}
	if len(servers) == 0 {
		bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownMCPServer, posUnknown,
			"%s.action.params.server: references %q but no MCP servers are declared in tools.modules.mcp.config.servers",
			path, server))
		return
	}
	d := diagnostic.Errorf(diagnostic.CodeUnknownMCPServer, posUnknown,
		"%s.action.params.server: unknown MCP server %q", path, server)
	if s, _ := suggest.Closest(server, mcpKeys(servers), 2); s != "" {
		d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
	}
	bag.Add(d)
}

func mcpKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
