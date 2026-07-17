package policy

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

type AvailableAction struct {
	Module string
	Action string
	Spec   *tool.Spec
	DiscoveryOnly bool
}

func BuildAgentToolset(
	appActive bool,
	caps *schema.CapabilitiesConfig,
	agent *schema.Agent,
	actions []AvailableAction,
) []AvailableAction {
	var agentModules map[string]AgentModuleAccess
	if agent != nil {
		agentModules = ResolveAgentModules(agent.Modules)
	}

	out := make([]AvailableAction, 0, len(actions))
	for _, a := range actions {
		inv := Invocation{
			Caller: CallerLLM,
			Module: a.Module,
			Action: a.Action,
		}
		pc := PolicyContext{
			AppActive:    appActive,
			Capabilities: caps,
			AgentModules: agentModules,
			ToolSpec:     a.Spec,
		}
		if !passesAllGates(inv, pc) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func passesAllGates(inv Invocation, pc PolicyContext) bool {
	for _, g := range gateChain {
		d := g(inv, pc)
		if d.Kind == DecisionDeny {
			return false
		}
	}
	return true
}

var gateChain = []func(Invocation, PolicyContext) Decision{
	Gate0Inactive,
	Gate1aModule,
	Gate1cMCPServer,
	Gate1bHidden,
	Gate2Risk,
	Gate3Permissions,
	Gate4Policy,
	Gate5Classification,
}

func ResolveAgentModules(mods schema.AgentModules) map[string]AgentModuleAccess {
	if len(mods) == 0 {
		return nil
	}
	out := make(map[string]AgentModuleAccess, len(mods))
	for _, ref := range mods {
		if ref.ID == "" {
			continue
		}
		access := out[ref.ID]
		if len(ref.Tools) == 0 {
			access.AllActions = true
		} else {
			if access.Actions == nil {
				access.Actions = make(map[string]struct{}, len(ref.Tools))
			}
			for _, t := range ref.Tools {
				access.Actions[t] = struct{}{}
			}
		}
		out[ref.ID] = access
	}
	return out
}
