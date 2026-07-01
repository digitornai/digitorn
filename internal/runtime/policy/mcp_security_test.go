package policy

import (
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// Capability policy (gate 4) written on the umbrella `mcp` module must cover
// every mcp_<server> virtual tool — exactly as a native module policy covers its
// actions. A policy on a specific mcp_<server> matches that server only.
func TestGate4Policy_MCPUmbrella(t *testing.T) {
	call := func(module, action string) Invocation {
		return Invocation{Caller: CallerLLM, Module: module, Action: action}
	}
	dec := func(caps *schema.CapabilitiesConfig, module, action string) DecisionKind {
		return Gate4Policy(call(module, action), PolicyContext{Capabilities: caps}).Kind
	}

	// deny on `mcp` blocks any mcp_<server> tool; native modules untouched.
	denyAll := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, Deny: []schema.CapabilityGrant{{Module: "mcp"}}}
	if dec(denyAll, "mcp_everything", "echo") != DecisionDeny {
		t.Error("deny on `mcp` must block mcp_everything.echo")
	}
	if dec(denyAll, "filesystem", "read") != DecisionAllow {
		t.Error("deny on `mcp` must NOT touch filesystem.read")
	}

	// deny on `mcp` with a tools list blocks only that action, across servers.
	denyTool := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, Deny: []schema.CapabilityGrant{{Module: "mcp", Tools: []string{"dangerous"}}}}
	if dec(denyTool, "mcp_x", "dangerous") != DecisionDeny {
		t.Error("deny mcp.dangerous must block mcp_x.dangerous")
	}
	if dec(denyTool, "mcp_x", "safe") != DecisionAllow {
		t.Error("deny mcp.dangerous must NOT block mcp_x.safe")
	}

	// approve on `mcp` gates mcp_<server> tools behind a human approval.
	appr := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, Approve: []schema.CapabilityGrant{{Module: "mcp"}}}
	if dec(appr, "mcp_everything", "echo") != DecisionNeedsApproval {
		t.Error("approve on `mcp` must gate mcp_everything.echo")
	}

	// grant on `mcp` allows under default_policy: approve (the grant matches).
	grant := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapApprove, Grant: []schema.CapabilityGrant{{Module: "mcp"}}}
	if dec(grant, "mcp_everything", "echo") != DecisionAllow {
		t.Error("grant on `mcp` must allow mcp_everything.echo despite default approve")
	}

	// Per-server exact policy still matches only that server.
	perServer := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, Deny: []schema.CapabilityGrant{{Module: "mcp_secret"}}}
	if dec(perServer, "mcp_secret", "x") != DecisionDeny {
		t.Error("per-server deny on mcp_secret must block it")
	}
	if dec(perServer, "mcp_other", "x") != DecisionAllow {
		t.Error("per-server deny on mcp_secret must NOT block mcp_other")
	}
}

// Gate 1c enforces tools.modules.mcp.constraints.allowed_servers at runtime: an
// MCP virtual tool (module mcp_<server>) of an unlisted server is denied, listed
// servers pass, native modules are untouched, and an empty (non-nil) set denies
// every MCP server. nil set = no restriction.
func TestGate1cMCPServer(t *testing.T) {
	llm := func(module string) Invocation {
		return Invocation{Caller: CallerLLM, Module: module, Action: "echo"}
	}

	if d := Gate1cMCPServer(llm("mcp_everything"), PolicyContext{}); d.Kind != DecisionAllow {
		t.Errorf("nil allowed_servers must allow, got %+v", d)
	}

	pc := PolicyContext{MCPAllowedServers: map[string]struct{}{"everything": {}}}
	if d := Gate1cMCPServer(llm("mcp_everything"), pc); d.Kind != DecisionAllow {
		t.Errorf("listed server must allow, got %+v", d)
	}
	if d := Gate1cMCPServer(llm("mcp_secret"), pc); d.Kind != DecisionDeny || d.Gate != GateMCPServer {
		t.Errorf("unlisted server must deny at gate1c, got %+v", d)
	}
	if d := Gate1cMCPServer(llm("filesystem"), pc); d.Kind != DecisionAllow {
		t.Errorf("non-mcp module must pass through the constraint, got %+v", d)
	}

	empty := PolicyContext{MCPAllowedServers: map[string]struct{}{}}
	if d := Gate1cMCPServer(llm("mcp_everything"), empty); d.Kind != DecisionDeny {
		t.Errorf("empty (non-nil) set must deny every mcp server, got %+v", d)
	}
}

// An MCP server's rate_limit_rpm is a MODULE-LEVEL cap: the limiter throttles the
// TOTAL calls to mcp_<server> across ALL its tools, leaving other servers and the
// action-level budgets independent.
func TestRateLimiter_ModuleLevel(t *testing.T) {
	now := time.Unix(2000, 0)
	lim := NewRateLimiter(map[string]int{"mcp_srv": 2}) // 2 calls/min for the whole server
	lim.now = func() time.Time { return now }

	if lim.Check("mcp_srv", "echo") != "" {
		t.Fatal("call 1 (echo) must pass")
	}
	if lim.Check("mcp_srv", "add") != "" {
		t.Fatal("call 2 (different tool) must pass — module budget is 2")
	}
	if lim.Check("mcp_srv", "ping") == "" {
		t.Fatal("call 3 (any tool) must be denied by the module aggregate")
	}
	if lim.Check("mcp_other", "echo") != "" {
		t.Fatal("a different server must not be limited")
	}
	now = now.Add(61 * time.Second)
	if lim.Check("mcp_srv", "echo") != "" {
		t.Fatal("after the window the server budget refills")
	}
}

// Module-level and action-level limits compose: BOTH must hold, and a denial by
// either records nothing (a blocked call never consumes budget).
func TestRateLimiter_ModuleAndActionCompose(t *testing.T) {
	now := time.Unix(3000, 0)
	lim := NewRateLimiter(map[string]int{"mcp_srv": 5, "mcp_srv.hot": 1})
	lim.now = func() time.Time { return now }

	if lim.Check("mcp_srv", "hot") != "" {
		t.Fatal("hot call 1 is within both limits")
	}
	if lim.Check("mcp_srv", "hot") == "" {
		t.Fatal("hot call 2 must be denied by the action limit")
	}
	// The denied hot call must NOT have consumed the module budget: 4 cold calls fit.
	for i := 0; i < 4; i++ {
		if lim.Check("mcp_srv", "cold") != "" {
			t.Fatalf("cold call %d must pass (module budget intact after denied hot)", i+1)
		}
	}
	// Module budget (5) now spent by 1 hot + 4 cold → next call denied.
	if lim.Check("mcp_srv", "cold") == "" {
		t.Fatal("module budget exhausted → deny")
	}
}
