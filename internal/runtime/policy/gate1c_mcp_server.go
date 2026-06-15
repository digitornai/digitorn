package policy

import "strings"

// Gate1cMCPServer enforces the per-app allow-list
// `tools.modules.mcp.constraints.allowed_servers`. MCP virtual tools are native
// tools whose module is mcp_<server>; when the app restricts which servers are
// callable, a tool belonging to a server NOT on the list is denied here — the
// same shape as gate 1a restricting modules. This closes the gap where the
// constraint compiled green but was never enforced at runtime.
//
// nil set (PolicyContext.MCPAllowedServers) = no restriction: every connected
// server is allowed. Non-MCP modules pass through untouched (the gate only
// looks at mcp_<server> module names). Non-LLM callers bypass like gate 1a.
func Gate1cMCPServer(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateMCPServer)
	}
	if pc.MCPAllowedServers == nil {
		return allow(GateMCPServer)
	}
	server, ok := mcpServerFromModule(inv.Module)
	if !ok {
		return allow(GateMCPServer) // not an MCP virtual-tool module
	}
	if _, allowed := pc.MCPAllowedServers[server]; !allowed {
		return deny(GateMCPServer,
			"MCP server "+server+" is not in the app's allowed_servers list")
	}
	return allow(GateMCPServer)
}

// mcpServerFromModule returns the server id of an mcp_<server> module and
// ok=false for any non-MCP module.
func mcpServerFromModule(module string) (string, bool) {
	if !strings.HasPrefix(module, "mcp_") {
		return "", false
	}
	return strings.TrimPrefix(module, "mcp_"), true
}

// moduleMatches reports whether a capabilities/hidden entry written for
// policyModule applies to the invoked invModule. It is an exact match, OR the
// MCP umbrella: an entry on the `mcp` module covers EVERY mcp_<server> virtual
// module — the concrete servers an app connects aren't known when the policy is
// authored (the same admission gate 1a gives the agent's `mcp` declaration). A
// policy written for a specific `mcp_<server>` still matches that server exactly.
// Used by gate 1b (hidden) and gate 4 (deny/approve/grant) so MCP virtual tools
// obey the app's capability policy exactly like native tools.
func moduleMatches(policyModule, invModule string) bool {
	return policyModule == invModule ||
		(policyModule == "mcp" && strings.HasPrefix(invModule, "mcp_")) ||
		(policyModule == "pieces" && strings.HasPrefix(invModule, "ap_"))
}
