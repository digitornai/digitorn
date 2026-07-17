package policy

import "strings"

func Gate1cMCPServer(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateMCPServer)
	}
	if pc.MCPAllowedServers == nil {
		return allow(GateMCPServer)
	}
	server, ok := mcpServerFromModule(inv.Module)
	if !ok {
		return allow(GateMCPServer)
	}
	if _, allowed := pc.MCPAllowedServers[server]; !allowed {
		return deny(GateMCPServer,
			"MCP server "+server+" is not in the app's allowed_servers list")
	}
	return allow(GateMCPServer)
}

func mcpServerFromModule(module string) (string, bool) {
	if !strings.HasPrefix(module, "mcp_") {
		return "", false
	}
	return strings.TrimPrefix(module, "mcp_"), true
}

func moduleMatches(policyModule, invModule string) bool {
	return policyModule == invModule ||
		(policyModule == "mcp" && strings.HasPrefix(invModule, "mcp_")) ||
		(policyModule == "pieces" && strings.HasPrefix(invModule, "ap_"))
}
