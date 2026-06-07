package validate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/compiler/suggest"
)

var mcpServerIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CheckMCPConfig hard-validates the tools.modules.mcp config block: unknown
// top-level keys, server id pattern, transport enum, required transport fields,
// deny-by-default sandbox on inline servers, timeout range, sandbox shape, and
// the allowed_servers cross-reference. Invalid MCP config fails compilation.
func CheckMCPConfig(file string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	if def.Tools == nil {
		return
	}
	block, ok := def.Tools.Modules["mcp"]
	if !ok {
		return
	}
	for k := range block.Config {
		switch k {
		case "workspace", "servers", "cache", "middleware":
		default:
			bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownField, posUnknown,
				"tools.modules.mcp.config.%s: unknown field (allowed: workspace, servers, cache, middleware)", k))
		}
	}
	servers, bad := schema.NormalizeServers(block.Config["servers"])
	for _, b := range bad {
		bag.Add(diagnostic.Errorf(diagnostic.CodeWrongType, posUnknown,
			"tools.modules.mcp.config.servers: malformed server entry %q", b))
	}
	ids := make([]string, 0, len(servers))
	for id := range servers {
		ids = append(ids, id)
	}
	for id, s := range servers {
		checkMCPServer(id, s, bag)
	}
	if raw, ok := block.Constraints["allowed_servers"]; ok {
		for _, name := range mcpStringList(raw) {
			if _, found := servers[name]; found {
				continue
			}
			d := diagnostic.Errorf(diagnostic.CodeUnknownMCPServer, posUnknown,
				"tools.modules.mcp.constraints.allowed_servers: unknown MCP server %q", name)
			if sug, _ := suggest.Closest(name, ids, 2); sug != "" {
				d = d.WithSuggestion(sug, fmt.Sprintf("did you mean %q?", sug))
			}
			bag.Add(d)
		}
	}
}

func checkMCPServer(id string, s schema.MCPServerConfig, bag *diagnostic.Bag) {
	path := "tools.modules.mcp.config.servers." + id
	if !mcpServerIDRe.MatchString(id) {
		bag.Add(diagnostic.Errorf(diagnostic.CodeBadRegex, posUnknown,
			"%s: server id %q must match ^[a-z][a-z0-9_]*$", path, id))
	}
	if s.Transport != "" && !mcpValidTransport(s.Transport) {
		d := diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
			"%s.transport: invalid transport %q (allowed: stdio, sse, streamable_http, http)", path, s.Transport)
		if sug, _ := suggest.Closest(string(s.Transport), mcpTransportNames(), 2); sug != "" {
			d = d.WithSuggestion(sug, fmt.Sprintf("did you mean %q?", sug))
		}
		bag.Add(d)
	}
	inline := s.Transport != "" || s.Command != "" || s.URL != ""
	if inline {
		switch mcpNormTransport(s.Transport) {
		case schema.MCPTransportStdio:
			if s.Command == "" {
				bag.Add(diagnostic.Errorf(diagnostic.CodeMissingRequired, posUnknown,
					"%s: stdio transport requires `command`", path))
			}
		case schema.MCPTransportSSE, schema.MCPTransportStreamableHTTP:
			if s.URL == "" {
				bag.Add(diagnostic.Errorf(diagnostic.CodeMissingRequired, posUnknown,
					"%s: %s transport requires `url`", path, mcpNormTransport(s.Transport)))
			}
		}
		if s.Sandbox == nil {
			bag.Add(diagnostic.Errorf(diagnostic.CodeMissingRequired, posUnknown,
				"%s: inline MCP server must declare a `sandbox` block (deny-by-default)", path))
		}
	}
	if s.Timeout != 0 && (s.Timeout < 1 || s.Timeout > 300) {
		bag.Add(diagnostic.Errorf(diagnostic.CodeOutOfRange, posUnknown,
			"%s.timeout: %.0f out of range [1, 300]", path, s.Timeout))
	}
	if s.Sandbox != nil {
		checkMCPSandbox(path+".sandbox", s.Sandbox, bag)
	}
}

func checkMCPSandbox(path string, sb *schema.MCPServerSandbox, bag *diagnostic.Bag) {
	for k := range sb.Extra {
		bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownField, posUnknown,
			"%s.%s: unknown field (allowed: permissions, paths, allowed_hosts)", path, k))
	}
	for _, p := range sb.Permissions {
		if !mcpValidPermission(p) {
			bag.Add(diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
				"%s.permissions: invalid permission %q (expected process.*, net.*, or fs.*)", path, p))
		}
	}
}

func mcpValidTransport(t schema.MCPTransport) bool {
	for _, a := range schema.AllMCPTransports {
		if a == t {
			return true
		}
	}
	return false
}

func mcpNormTransport(t schema.MCPTransport) schema.MCPTransport {
	switch t {
	case "":
		return schema.MCPTransportStdio
	case schema.MCPTransportHTTP:
		return schema.MCPTransportStreamableHTTP
	default:
		return t
	}
}

func mcpTransportNames() []string {
	out := make([]string, len(schema.AllMCPTransports))
	for i, t := range schema.AllMCPTransports {
		out[i] = string(t)
	}
	return out
}

func mcpValidPermission(p string) bool {
	for _, pre := range []string{"process.", "net.", "fs."} {
		if strings.HasPrefix(p, pre) && len(p) > len(pre) {
			return true
		}
	}
	return false
}

func mcpStringList(raw any) []string {
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}

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
