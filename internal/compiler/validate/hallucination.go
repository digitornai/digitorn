package validate

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/parse"
	"github.com/mbathepaul/digitorn/internal/compiler/position"
)

type hallucinationHint struct {
	wrongKey  string
	rightKey  string
	rightPath string
	reason    string
}

var topLevelHints = []hallucinationHint{
	{wrongKey: "system", rightKey: "system_prompt", rightPath: "agents[].system_prompt", reason: "top-level 'system' is not a digitorn key — use the agent's system_prompt"},
	{wrongKey: "model", rightKey: "model", rightPath: "agents[].brain.model", reason: "top-level 'model' is not a digitorn key — set it inside agents[].brain"},
	{wrongKey: "provider", rightKey: "provider", rightPath: "agents[].brain.provider", reason: "top-level 'provider' is not a digitorn key — set it inside agents[].brain"},
	{wrongKey: "temperature", rightKey: "temperature", rightPath: "agents[].brain.temperature", reason: "top-level 'temperature' belongs inside agents[].brain"},
	{wrongKey: "max_tokens", rightKey: "max_tokens", rightPath: "agents[].brain.max_tokens", reason: "top-level 'max_tokens' belongs inside agents[].brain"},
	{wrongKey: "agent", rightKey: "agents", rightPath: "agents (list)", reason: "use the plural 'agents:' as a list of agent definitions"},
	{wrongKey: "module", rightKey: "tools.modules", rightPath: "tools.modules", reason: "modules live under 'tools.modules' (or top-level 'modules:' alias) — 'module:' singular is not valid"},
	{wrongKey: "prompt", rightKey: "system_prompt", rightPath: "agents[].system_prompt", reason: "top-level 'prompt' is not a digitorn key — set it inside an agent as system_prompt"},
	{wrongKey: "instructions", rightKey: "system_prompt", rightPath: "agents[].system_prompt", reason: "top-level 'instructions' is not a digitorn key — use agents[].system_prompt or agents[].instructions"},
	{wrongKey: "hook", rightKey: "hooks", rightPath: "runtime.hooks or agents[].hooks", reason: "use the plural 'hooks:' list under runtime or an agent"},
	{wrongKey: "tool", rightKey: "tools", rightPath: "tools (with .modules)", reason: "top-level singular 'tool:' is not a key — declare modules under tools.modules"},
}

func CheckHallucinations(file string, doc *yaml.Node, bag *diagnostic.Bag) {
	if !parse.IsMapping(doc) {
		return
	}
	for _, h := range topLevelHints {
		keyNode, _, ok := parse.FindKey(doc, h.wrongKey)
		if !ok {
			continue
		}
		pos := position.Pos{File: file, Line: keyNode.Line, Column: keyNode.Column}
		d := diagnostic.Warningf(diagnostic.CodeUnknownField, pos,
			"top-level %q is likely a typo — %s", h.wrongKey, h.reason)
		d = d.WithSuggestion(h.rightPath, fmt.Sprintf("place under %s", h.rightPath))
		bag.Add(d)
	}
	checkAgentHallucinations(file, doc, bag)
	checkHookHallucinations(file, doc, bag)
}

func checkAgentHallucinations(file string, doc *yaml.Node, bag *diagnostic.Bag) {
	_, agentsNode, ok := parse.FindKey(doc, "agents")
	if !ok || agentsNode.Kind != yaml.SequenceNode {
		return
	}
	for idx, a := range agentsNode.Content {
		if !parse.IsMapping(a) {
			continue
		}
		for _, wrong := range []string{"system", "model", "provider", "temperature", "max_tokens", "top_p"} {
			keyNode, _, ok := parse.FindKey(a, wrong)
			if !ok {
				continue
			}
			switch wrong {
			case "system":
				bag.Add(diagnostic.Warningf(diagnostic.CodeUnknownField,
					position.Pos{File: file, Line: keyNode.Line, Column: keyNode.Column},
					"agents.%d: %q should be %q", idx, "system", "system_prompt").
					WithSuggestion("system_prompt", "use 'system_prompt:' to set the agent's system message"))
			case "model", "provider", "temperature", "max_tokens", "top_p":
				bag.Add(diagnostic.Warningf(diagnostic.CodeUnknownField,
					position.Pos{File: file, Line: keyNode.Line, Column: keyNode.Column},
					"agents.%d: %q belongs inside the 'brain:' block", idx, wrong).
					WithSuggestion("brain."+wrong, fmt.Sprintf("move under brain.%s", wrong)))
			}
		}
	}
}

func checkHookHallucinations(file string, doc *yaml.Node, bag *diagnostic.Bag) {
	_, runtime, ok := parse.FindKey(doc, "runtime")
	if !ok {
		return
	}
	_, hooks, ok := parse.FindKey(runtime, "hooks")
	if !ok || hooks.Kind != yaml.SequenceNode {
		return
	}
	for i, h := range hooks.Content {
		if !parse.IsMapping(h) {
			continue
		}
		checkHookConditionShape(file, fmt.Sprintf("runtime.hooks.%d", i), h, bag)
	}
}

func checkHookConditionShape(file, path string, hook *yaml.Node, bag *diagnostic.Bag) {
	for _, key := range []string{"condition", "action"} {
		keyNode, valNode, ok := parse.FindKey(hook, key)
		if !ok || valNode == nil {
			continue
		}
		if valNode.Kind == yaml.ScalarNode {
			bag.Add(diagnostic.Warningf(diagnostic.CodeWrongType,
				position.Pos{File: file, Line: keyNode.Line, Column: keyNode.Column},
				"%s.%s: expected a mapping with 'type:' (e.g. %s: { type: ... }), got scalar %q",
				path, key, key, valNode.Value))
		}
	}
}
