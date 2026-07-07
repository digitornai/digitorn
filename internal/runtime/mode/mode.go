// Package mode resolves the effective per-turn configuration for a chosen
// composer mode (runtime.modes in app YAML) and partitions the agent's
// tools into allowed/blocked sets for that mode.
//
// This is a faithful port of the reference daemon's mode_merge.py, with a
// few deliberate hardening fixes called out inline :
//
//   - system modules + meta-tools are NEVER blocked by a mode (they bypass
//     the security gates too — blocking execute_tool/search_tools would
//     break discovery mode) ;
//   - max_turns / timeout of 0 fall back to the app default (matching
//     Python's truthiness `or`, and because 0 is nonsensical) ;
//   - a module that appears in grants both as a whole-module grant and an
//     action-scoped grant resolves deterministically to "whole module
//     allowed" (Python was order-dependent).
package mode

import (
	"sort"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/toolname"
)

type EffectiveTurn struct {
	ActiveModeID string
	ModeLabel    string
	MaxTurns     int
	Timeout      float64

	SystemPromptSuffix string

	ToolGrants      []schema.CapabilityGrant
	BehaviorProfile string
	WorkspaceMode   string
}

func (e EffectiveTurn) Filtered() bool { return e.ToolGrants != nil }


type AppDefaults struct {
	MaxTurns            int
	Timeout             float64
	BaseBehaviorProfile string
}


func DefaultModeID(modes map[string]schema.ModeDef, order []string) string {
	if len(modes) == 0 {
		return ""
	}
	if _, ok := modes["auto"]; ok {
		return "auto"
	}
	for _, id := range order {
		if _, ok := modes[id]; ok {
			return id
		}
	}
	return ""
}


func Resolve(modes map[string]schema.ModeDef, order []string, modeID string, def AppDefaults) EffectiveTurn {
	resolvedID := modeID
	if resolvedID == "" {
		resolvedID = DefaultModeID(modes, order)
	}
	md, ok := modes[resolvedID]
	if resolvedID == "" || !ok {
		return EffectiveTurn{
			MaxTurns:        def.MaxTurns,
			Timeout:         def.Timeout,
			BehaviorProfile: def.BaseBehaviorProfile,
		}
	}

	grants := md.ToolGrants
	if len(grants) == 0 {
		grants = nil
	}

	label := md.Label
	if label == "" {
		label = capitalize(resolvedID)
	}

	maxTurns := def.MaxTurns
	if md.MaxTurns != nil && *md.MaxTurns > 0 {
		maxTurns = *md.MaxTurns
	}
	timeout := def.Timeout
	if md.Timeout != nil && *md.Timeout > 0 {
		timeout = *md.Timeout
	}
	profile := md.BehaviorProfile
	if profile == "" {
		profile = def.BaseBehaviorProfile
	}
	ws := ""
	if md.WorkspaceMode != nil {
		ws = *md.WorkspaceMode
	}

	return EffectiveTurn{
		ActiveModeID:       resolvedID,
		ModeLabel:          label,
		MaxTurns:           maxTurns,
		Timeout:            timeout,
		SystemPromptSuffix: md.SystemPrompt,
		ToolGrants:         grants,
		BehaviorProfile:    profile,
		WorkspaceMode:      ws,
	}
}


func ComputeToolPartition(grants []schema.CapabilityGrant, offered []string) (allowed, blocked map[string]struct{}) {
	allowed = make(map[string]struct{}, len(offered))
	blocked = make(map[string]struct{})
	if grants == nil {
		for _, n := range offered {
			allowed[n] = struct{}{}
		}
		return allowed, blocked
	}

	whole := make(map[string]bool)                 // module -> whole module allowed
	scoped := make(map[string]map[string]struct{}) // module -> allowed actions
	for _, g := range grants {
		if g.Module == "" {
			continue
		}
		actions := g.EffectiveTools()
		if len(actions) == 0 {
			whole[g.Module] = true
			continue
		}
		set := scoped[g.Module]
		if set == nil {
			set = make(map[string]struct{}, len(actions))
			scoped[g.Module] = set
		}
		for _, a := range actions {
			set[a] = struct{}{}
		}
	}

	for _, name := range offered {
		module, action := splitFQN(name)

		if policy.IsSystemModule(module) || policy.IsMetaTool(action) || policy.IsRuntimeInternalModule(module) {
			allowed[name] = struct{}{}
			continue
		}
		if whole[module] {
			allowed[name] = struct{}{}
			continue
		}
		if set, ok := scoped[module]; ok {
			if _, ok := set[action]; ok {
				allowed[name] = struct{}{}
				continue
			}
		}
		blocked[name] = struct{}{}
	}
	return allowed, blocked
}

func BuildModeSwitchMessage(eff EffectiveTurn, allowed, blocked map[string]struct{}) string {
	label := eff.ModeLabel
	if label == "" {
		label = eff.ActiveModeID
	}
	parts := make([]string, 0, 5)
	if label != "" {
		parts = append(parts, "[Mode: "+label+"]")
	} else {
		parts = append(parts, "[Mode]")
	}
	if body := strings.TrimSpace(eff.SystemPromptSuffix); body != "" {
		parts = append(parts, body)
	}
	if len(blocked) == 0 {
		parts = append(parts, "All tools are available in this mode.")
	} else {
		allowedLine := strings.Join(sortedKeys(allowed), ", ")
		if allowedLine == "" {
			allowedLine = "(none)"
		}
		parts = append(parts,
			"Tools available in this mode: "+allowedLine,
			"Tools blocked in this mode: "+strings.Join(sortedKeys(blocked), ", "),
			"If you need a blocked tool, ask the user to switch to a mode that allows it. Do not retry a blocked tool.",
		)
	}
	return strings.Join(parts, "\n\n")
}


func splitFQN(name string) (module, action string) {
	fqn := toolname.Canonicalize(name)
	if i := strings.IndexByte(fqn, '.'); i >= 0 {
		return fqn[:i], fqn[i+1:]
	}
	return fqn, ""
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}


func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
