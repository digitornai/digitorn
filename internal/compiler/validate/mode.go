package validate

import (
	"sort"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// triggerAdapters are the channel adapter types that double as event sources
// (i.e. they trigger sessions, not just deliver notifications).
var triggerAdapters = map[string]struct{}{
	"cron": {}, "watch": {}, "file_watcher": {}, "http": {}, "webhook": {},
	"gmail": {}, "email": {}, "telegram": {}, "discord": {}, "slack": {},
	"voice_twilio": {}, "voice_websocket": {}, "voice": {},
	"kafka": {}, "rss": {}, "sms": {}, "queue": {},
}

func hasChannelTrigger(def *schema.AppDefinition) bool {
	if def.Tools == nil {
		return false
	}
	mod, ok := def.Tools.Modules["channels"]
	if !ok {
		return false
	}
	providers, _ := mod.Config["providers"].(map[string]any)
	for _, raw := range providers {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		adapter, _ := p["adapter"].(string)
		if _, ok := triggerAdapters[adapter]; ok {
			return true
		}
	}
	return false
}

func (v *validator) checkMode() {
	if v.def.Runtime == nil {
		return
	}
	rt := v.def.Runtime
	m := mode(v.def)

	if m != "background" && len(rt.Triggers) > 0 {
		v.errf(diagnostic.CodeModeFieldMisuse, "runtime.triggers",
			"triggers are only valid in mode: background (current mode: %s)", m)
	}
	if m == "background" && len(rt.Triggers) == 0 && !hasChannelTrigger(v.def) {
		v.errf(diagnostic.CodeMissingTrigger, "runtime",
			"mode: background requires at least one trigger (runtime.triggers or a cron/watch/http channel adapter)")
	}

	if m != "one_shot" {
		if rt.Input != nil {
			v.errf(diagnostic.CodeBadInputOutput, "runtime.input",
				"runtime.input is only valid in mode: one_shot (current mode: %s)", m)
		}
		if rt.Output != nil {
			v.errf(diagnostic.CodeBadInputOutput, "runtime.output",
				"runtime.output is only valid in mode: one_shot (current mode: %s)", m)
		}
	}

	if m != "pipeline" && len(rt.Pipeline) > 0 {
		v.errf(diagnostic.CodeModeFieldMisuse, "runtime.pipeline",
			"runtime.pipeline is only valid in mode: pipeline (current mode: %s)", m)
	}
	if m == "pipeline" && len(rt.Pipeline) == 0 {
		v.errf(diagnostic.CodeBadInputOutput, "runtime.pipeline",
			"mode: pipeline requires at least one pipeline step")
	}

	if (m == "one_shot" || m == "pipeline") && rt.EntryAgent == "" && len(v.def.Agents) > 1 && !hasCoordinator(v.def) {
		v.errf(diagnostic.CodeNoEntryAgent, "runtime",
			"mode: %s with multiple agents requires runtime.entry_agent or a coordinator role", m)
	}
}

// knownModeIcons / knownModeAccents are the picker hints the client renders
// natively. Unknown values are not errors — the client falls back — so they
// surface as warnings (the YAML still compiles).
var knownModeIcons = map[string]struct{}{
	"lightbulb": {}, "map": {}, "sparkles": {}, "wrench": {}, "shield": {},
}

var knownModeAccents = map[string]struct{}{
	"primary": {}, "secondary": {}, "cyan": {}, "purple": {},
	"red": {}, "green": {}, "orange": {},
}

// checkComposerModes validates runtime.modes (the composer mode picker),
// distinct from runtime.mode (the execution mode checked by checkMode).
// Bounds (max_turns >= 1, timeout > 0) are hard constraints (errors) ; icon /
// accent are UI hints (warnings on unknown values).
func (v *validator) checkComposerModes() {
	if v.def.Runtime == nil || len(v.def.Runtime.Modes) == 0 {
		return
	}
	rt := v.def.Runtime
	for _, id := range composerModeIDs(rt) {
		md := rt.Modes[id]
		base := "runtime.modes." + id

		if md.Icon != "" {
			if _, ok := knownModeIcons[md.Icon]; !ok {
				v.warnf(diagnostic.CodeUnknownEnumHint, base+".icon",
					"unknown mode icon %q (known: lightbulb, map, sparkles, wrench, shield); the client will pick a default", md.Icon)
			}
		}
		if md.Accent != "" {
			if _, ok := knownModeAccents[md.Accent]; !ok {
				v.warnf(diagnostic.CodeUnknownEnumHint, base+".accent",
					"unknown mode accent %q (known: primary, secondary, cyan, purple, red, green, orange); the client will fall back to the theme accent", md.Accent)
			}
		}
		if md.MaxTurns != nil && *md.MaxTurns < 1 {
			v.errf(diagnostic.CodeOutOfRange, base+".max_turns",
				"max_turns must be >= 1 (got %d)", *md.MaxTurns)
		}
		if md.Timeout != nil && *md.Timeout <= 0 {
			v.errf(diagnostic.CodeOutOfRange, base+".timeout",
				"timeout must be > 0 (got %g)", *md.Timeout)
		}
	}
}

// composerModeIDs returns the mode ids in their captured YAML order, with any
// keys missing from the order list appended sorted (deterministic diagnostics).
func composerModeIDs(rt *schema.RuntimeBlock) []string {
	seen := make(map[string]struct{}, len(rt.Modes))
	order := make([]string, 0, len(rt.Modes))
	for _, id := range rt.ModesOrder {
		if _, ok := rt.Modes[id]; ok {
			if _, dup := seen[id]; !dup {
				order = append(order, id)
				seen[id] = struct{}{}
			}
		}
	}
	rest := make([]string, 0, len(rt.Modes))
	for id := range rt.Modes {
		if _, ok := seen[id]; !ok {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

func hasCoordinator(def *schema.AppDefinition) bool {
	for _, a := range def.Agents {
		if a.Role == "coordinator" {
			return true
		}
	}
	return false
}
