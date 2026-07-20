package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/mode"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/toolname"
	"github.com/digitornai/digitorn/internal/runtime/turn"
)

// modeGuard is the per-turn tool allow-list enforced at dispatch time
// (defense in depth behind the LLM-schema filter). nil = no mode filtering.
// allowed holds CANONICAL tool names ; the dispatcher gates calls whose
// canonical name is absent.
type modeGuard struct {
	allowed     map[string]struct{}
	label       string
	allowedList string // sorted, human-readable, for the synthetic error
}

// blocks reports whether a canonical tool name is outside the mode's allow-list.
func (g *modeGuard) blocks(canonicalName string) bool {
	if g == nil {
		return false
	}
	_, ok := g.allowed[canonicalName]
	return !ok
}

// blockedError is the doc-conform synthetic error for a mode-blocked tool.
func (g *modeGuard) blockedError(toolName string) string {
	return fmt.Sprintf(
		"Tool %q is blocked in mode %q. Allowed tools: %s. Ask the user to switch to a mode that allows this tool. Do not retry this call.",
		toolName, g.label, g.allowedList)
}

type modeGuardCtxKey struct{}

// withModeGuard carries the active mode guard down the dispatch ctx so the
// shared sub-tool chokepoint (enforceGate) can enforce the mode allow-list on
// tools reached through meta paths (execute_tool / run_parallel /
// background_run), not just on top-level calls. nil guard = no-op.
func withModeGuard(ctx context.Context, g *modeGuard) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, modeGuardCtxKey{}, g)
}

func modeGuardFromCtx(ctx context.Context) *modeGuard {
	g, _ := ctx.Value(modeGuardCtxKey{}).(*modeGuard)
	return g
}

// applyTurnMode resolves the composer mode for this turn and enforces it :
//
//   - filters the offered tool list to the mode's allow-list (what the LLM
//     sees) ;
//   - returns a modeGuard for the dispatch-time check (hallucinated / stale
//     calls to blocked tools) ;
//   - announces a mode switch as a durable system directive when the active
//     mode changed, re-snapshotting so it lands in THIS turn's context ;
//   - returns the per-turn max_turns / timeout caps (0 = no override).
//
// Inert (nil guard, 0 caps) when the app declares no modes or the request
// resolves to no mode.
func (e *Engine) applyTurnMode(
	ctx context.Context,
	tr *turn.Turn,
	app *appmgr.RuntimeApp,
	in TurnInput,
	snap *sessionstore.SessionSnapshot,
	tools *[]llm.ToolSpec,
) (guard *modeGuard, maxTurns int, timeout float64, behaviorProfile, modePrompt string) {
	var (
		modes      map[string]schema.ModeDef
		order           []string
		declaredDefault string
		defMax          int
		defTimeout      float64
	)
	if app != nil && app.Definition != nil && app.Definition.Runtime != nil {
		rt := app.Definition.Runtime
		modes = rt.Modes
		order = rt.ModesOrder
		declaredDefault = rt.DefaultMode
		defMax = rt.MaxTurns
		defTimeout = rt.Timeout
	}
	if len(modes) == 0 {
		return nil, 0, 0, "", ""
	}

	// Stickiness : an omitted mode reuses the session's active mode before
	// falling back to the app default-policy.
	requested := in.Mode
	if requested == "" {
		requested = snap.ActiveMode
	}
	eff := mode.Resolve(modes, order, requested, mode.AppDefaults{
		DefaultMode: declaredDefault,
		MaxTurns:    defMax,
		Timeout:     defTimeout,
	})
	if eff.ActiveModeID == "" {
		return nil, 0, 0, "", ""
	}

	offered := make([]string, 0, len(*tools))
	for _, t := range *tools {
		offered = append(offered, t.Name)
	}
	allowed, blocked := mode.ComputeToolPartition(eff.ToolGrants, offered)

	if eff.Filtered() {
		kept := make([]llm.ToolSpec, 0, len(allowed))
		for _, t := range *tools {
			if _, ok := allowed[t.Name]; ok {
				kept = append(kept, t)
			}
		}
		*tools = kept

		canon := make(map[string]struct{}, len(allowed))
		for n := range allowed {
			canon[toolname.Canonicalize(n)] = struct{}{}
		}
		guard = &modeGuard{
			allowed:     canon,
			label:       eff.ModeLabel,
			allowedList: sortedJoin(allowed),
		}
	}

	// Announce the switch (durable directive + re-snapshot) only when the
	// active mode actually changed for this session.
	if eff.ActiveModeID != snap.ActiveMode {
		msg := mode.BuildModeSwitchMessage(eff, allowed, blocked)
		e.injectSystemDirective(ctx, in, tr.ID, msg, DirectiveModeSwitch, map[string]any{
			"mode_id":    eff.ActiveModeID,
			"mode_label": eff.ModeLabel,
			// behavior_profile is the seam for the (separate) behavior engine
			// to re-resolve its active rules ; carried here so it is durable
			// + observable even before that engine is wired per-turn.
			"behavior_profile": eff.BehaviorProfile,
		}, nil)
		if st, err := e.Sessions.State(in.SessionID); err == nil && st != nil {
			*snap = st.Snapshot()
		}
	}

	// The mode's system_prompt goes back to the caller so it can be folded into
	// the turn's system prompt EVERY turn. It used to travel only inside the
	// mode-switch directive: a durable transcript message, which ApplyView drops
	// once its seq falls before the compaction cutoff. The picker kept showing
	// the mode while its instructions had silently left the model's context.
	return guard, eff.MaxTurns, eff.Timeout, eff.BehaviorProfile, eff.SystemPromptSuffix
}

func sortedJoin(set map[string]struct{}) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
