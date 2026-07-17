package policy

import (
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

type CallerKind int

const (
	CallerUnknown CallerKind = iota
	CallerLLM
	CallerHook
	CallerSetup
	CallerChannel
	CallerInternal
)

func (c CallerKind) IsLLM() bool { return c == CallerLLM }

func (c CallerKind) String() string {
	switch c {
	case CallerLLM:
		return "llm"
	case CallerHook:
		return "hook"
	case CallerSetup:
		return "setup"
	case CallerChannel:
		return "channel"
	case CallerInternal:
		return "internal"
	default:
		return "unknown"
	}
}

type DecisionKind int

const (
	DecisionAllow DecisionKind = iota
	DecisionDeny
	DecisionNeedsApproval
)

func (d DecisionKind) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionNeedsApproval:
		return "needs_approval"
	default:
		return "unknown"
	}
}

type GateCode string

const (
	GateInactive       GateCode = "gate0_inactive"
	GateModule         GateCode = "gate1a_module"
	GateMCPServer      GateCode = "gate1c_mcp_server"
	GateHidden         GateCode = "gate1b_hidden"
	GateRisk           GateCode = "gate2_risk"
	GatePermissions    GateCode = "gate3_permissions"
	GatePolicy         GateCode = "gate4_policy"
	GateClassification GateCode = "gate5_classification"
	GateRateLimit      GateCode = "gate6_rate_limit"
)

type Decision struct {
	Kind   DecisionKind
	Gate   GateCode
	Reason string
}

func (d Decision) IsDeny() bool { return d.Kind == DecisionDeny }

func (d Decision) IsBlocking() bool {
	return d.Kind == DecisionDeny || d.Kind == DecisionNeedsApproval
}

func allow(gate GateCode) Decision {
	return Decision{Kind: DecisionAllow, Gate: gate}
}

func deny(gate GateCode, reason string) Decision {
	return Decision{Kind: DecisionDeny, Gate: gate, Reason: reason}
}

func needsApproval(gate GateCode, reason string) Decision {
	return Decision{Kind: DecisionNeedsApproval, Gate: gate, Reason: reason}
}

type Invocation struct {
	Caller    CallerKind
	AppID     string
	AgentID   string
	SessionID string
	UserID    string

	Module string
	Action string
}

func (i Invocation) FQN() string {
	if i.Module == "" {
		return i.Action
	}
	if i.Action == "" {
		return i.Module
	}
	return i.Module + "." + i.Action
}

type PolicyContext struct {
	AppActive    bool
	Capabilities *schema.CapabilitiesConfig

	AgentModules map[string]AgentModuleAccess

	ToolSpec *tool.Spec

	RateLimiter *RateLimiter

	MCPAllowedServers map[string]struct{}
}

type AgentModuleAccess struct {
	AllActions bool
	Actions    map[string]struct{}
}

func (c PolicyContext) CanAgentCall(module, action string) bool {
	if c.AgentModules == nil {
		return true
	}
	access, ok := c.AgentModules[module]
	if !ok {
		if strings.HasPrefix(module, "mcp_") {
			access, ok = c.AgentModules["mcp"]
		}
		if !ok && strings.HasPrefix(module, "ap_") {
			access, ok = c.AgentModules["pieces"]
		}
		if !ok {
			return false
		}
	}
	if access.AllActions {
		return true
	}
	_, ok = access.Actions[action]
	return ok
}
