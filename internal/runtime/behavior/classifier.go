package behavior

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const classifierSystemPrompt = `You are a behavioral director for an AI agent. You analyze what the user wants, what the agent has done so far, and what tools it has - then you produce precise directives that push the agent toward optimal behavior.

You are NOT a simple task categorizer. You are a coach who understands the domain and directs the agent's behavior in real-time.

## What you receive

1. **User message** - what the user just asked
2. **Agent tools** - the exact tools available with short descriptions
3. **Session state** - what the agent has already done: files read, edits, tests, violations, turn number
4. **Workspace context** - project type, languages, scale, recent activity
5. **Active rules** - behavioral rules being enforced at runtime
6. **Recent history** - last few messages with tool calls and results
7. **Profile context** - custom behavioral instructions from the app author

## Your output

%s

Return ` + "`{\"skip_reason\": \"...\"}`" + ` with empty directives when classification is unnecessary (simple follow-up, agent already on track, trivial acknowledgment).

## Behavioral model

### Phase 1: UNDERSTAND (before any action)
- What is the user actually trying to achieve? (intent, not literal words)
- How big is this? (1 step? 5 steps? cross-cutting?)
- What could go wrong? (data loss, breaking changes, security)
- Does the agent know enough to start?

### Phase 2: SCOPE & PLAN
%s

### Phase 3: EXECUTE with discipline
- Search BEFORE reading, Read BEFORE editing, Plan BEFORE implementing, Verify AFTER changing
- Delegate WHEN appropriate, Ask WHEN uncertain

### Phase 4: VERIFY
- After every change: did it come out right? Any errors?
- Before answering: did the agent check the actual current state?

## How to write directives

Directives are BEHAVIORAL COMMANDS, not task instructions. The agent already knows WHAT to do - you tell it HOW to approach it.

%s

## When to skip (return skip_reason)

- "yes", "ok", "continue", "go ahead" -> agent already on track
- Simple question needing 1 action -> trivial
- No task content -> skip`

func defaultClassifierConfig() map[string]any {
	return map[string]any{
		"frequency":      "every_turn",
		"frequency_n":    3,
		"skip_followups": true,
		"timeout":        15,
		"complexity_levels": []any{
			map[string]any{"name": "trivial", "when": "1 action, obvious answer", "behavior": "Just do it"},
			map[string]any{"name": "simple", "when": "2-4 actions, clear path", "behavior": "Search, read, act, verify"},
			map[string]any{"name": "moderate", "when": "5-15 actions, multi-step", "behavior": "Explore structure, plan, confirm with user, implement, verify"},
			map[string]any{"name": "complex", "when": "15+ actions, multi-file or cross-cutting", "behavior": "Explore via sub-agents, detailed plan, user approval, phased implementation, test each phase"},
			map[string]any{"name": "critical", "when": "Destructive, migrations, security, production", "behavior": "ALWAYS confirm with user first, explain risks, get explicit approval"},
		},
		"approaches": []any{
			map[string]any{"name": "direct", "label": "Execute directly", "when": "Task is trivial or simple with a clear path", "behavior": "Go straight to tool calls, minimal planning text"},
			map[string]any{"name": "explore_first", "label": "Explore first, then act", "when": "Agent needs to understand the codebase/context before acting", "behavior": "Search and read before making any changes, summarize findings"},
			map[string]any{"name": "plan_and_confirm", "label": "Plan and get user approval", "when": "Task touches 3+ files or has multiple valid approaches", "behavior": "Write a numbered plan with file paths, present to user, wait for approval"},
			map[string]any{"name": "delegate", "label": "Delegate to sub-agents", "when": "Task has independent sub-tasks or requires bulk exploration", "behavior": "Launch sub-agents in parallel, collect results, then synthesize"},
			map[string]any{"name": "research_first", "label": "Research before implementing", "when": "Unknown APIs, unfamiliar tech, ambiguous requirements", "behavior": "Search the web or docs first, verify assumptions, then implement"},
		},
		"risk_levels": []any{
			map[string]any{"name": "none", "when": "Read-only, questions, exploration"},
			map[string]any{"name": "low", "when": "Small edits, new files, safe commands"},
			map[string]any{"name": "medium", "when": "Multiple file edits, dependency changes, config modifications"},
			map[string]any{"name": "high", "when": "Deletions, migrations, security-related, production configs, irreversible operations", "behavior": "MUST confirm with user, explain what could go wrong"},
		},
		"max_directives": 5,
		"context": map[string]any{
			"tool_inventory": true,
			"session_state":  true,
			"workspace_info": true,
			"recent_history": true,
			"history_depth":  8,
		},
		"directive_prefix":    "[BEHAVIOR DIRECTIVE - {complexity} complexity, {risk} risk]",
		"high_risk_warning":   "Risk level: {risk}. Confirm destructive or irreversible actions with the user before proceeding.",
		"high_risk_threshold": "medium",
		"directive_footer":    "Follow these directives. They are based on your behavioral rules and the current session state. Violations are detected in real-time.",
	}
}

func classifierGet(cfg map[string]any, key string) any {
	if cfg != nil {
		if v, ok := cfg[key]; ok {
			return v
		}
	}
	return defaultClassifierConfig()[key]
}

func classifierGetStr(cfg map[string]any, key string) string {
	s, _ := classifierGet(cfg, key).(string)
	return s
}

func classifierGetInt(cfg map[string]any, key string, def int) int {
	return intFromAny(classifierGet(cfg, key), def)
}

func classifierContextOn(cfg map[string]any, key string) bool {
	if cfg != nil {
		if ctx, ok := cfg["context"].(map[string]any); ok {
			if v, ok := ctx[key]; ok {
				return truthy(v)
			}
		}
	}
	def, _ := defaultClassifierConfig()["context"].(map[string]any)
	if v, ok := def[key]; ok {
		return truthy(v)
	}
	return true
}

func normalizeEntry(e any) map[string]any {
	switch v := e.(type) {
	case map[string]any:
		return v
	case string:
		return map[string]any{"name": v}
	default:
		return map[string]any{"name": fmt.Sprintf("%v", e)}
	}
}

func entryName(e any) string {
	m := normalizeEntry(e)
	if n, ok := m["name"].(string); ok {
		return n
	}
	return fmt.Sprintf("%v", m["name"])
}

func entryLabel(m map[string]any) string {
	if l, ok := m["label"].(string); ok && l != "" {
		return l
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}

func asList(cfg map[string]any, key string) []any {
	l, _ := classifierGet(cfg, key).([]any)
	return l
}

func buildSystemPrompt(cfg map[string]any) string {
	if cp := classifierGetStr(cfg, "system_prompt"); cp != "" {
		return cp
	}
	c := mapEntries(asList(cfg, "complexity_levels"))
	a := mapEntries(asList(cfg, "approaches"))
	r := mapEntries(asList(cfg, "risk_levels"))
	maxD := classifierGetInt(cfg, "max_directives", 5)

	schema := buildOutputSchema(c, a, r, maxD)
	guide := buildGuide("Complexity levels", c) + "\n\n" + buildGuide("Approaches", a)
	risk := buildGuide("Risk assessment", r)
	return fmt.Sprintf(classifierSystemPrompt, schema, guide, risk)
}

func mapEntries(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, normalizeEntry(e))
	}
	return out
}

func buildOutputSchema(complexity, approaches, risk []map[string]any, maxD int) string {
	enum := func(items []map[string]any) string {
		parts := make([]string, 0, len(items))
		for _, e := range items {
			parts = append(parts, fmt.Sprintf("%q", e["name"]))
		}
		return strings.Join(parts, " | ")
	}
	return "A JSON object:\n```json\n{\n" +
		fmt.Sprintf("  \"complexity\": %s,\n", enum(complexity)) +
		fmt.Sprintf("  \"approach\": %s,\n", enum(approaches)) +
		fmt.Sprintf("  \"risk_level\": %s,\n", enum(risk)) +
		fmt.Sprintf("  \"directives\": [\"directive 1\", ...] // max %d\n", maxD) +
		"}\n```"
}

func buildGuide(title string, items []map[string]any) string {
	lines := []string{"## " + title}
	for _, item := range items {
		name, _ := item["name"].(string)
		if name == "" {
			name = "?"
		}
		line := "**" + name + "**"
		if label, _ := item["label"].(string); label != "" {
			line += " (" + label + ")"
		}
		if when, _ := item["when"].(string); when != "" {
			line += "\n  When: " + when
		}
		if beh, _ := item["behavior"].(string); beh != "" {
			line += "\n  Agent behavior: " + beh
		}
		lines = append(lines, "- "+line)
	}
	return strings.Join(lines, "\n")
}

func approachLabel(cfg map[string]any, name string) string {
	for _, e := range asList(cfg, "approaches") {
		m := normalizeEntry(e)
		if m["name"] == name {
			return entryLabel(m)
		}
	}
	return name
}

func entryNames(list []any) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, entryName(e))
	}
	return out
}

var followupPattern = regexp.MustCompile(`(?i)^(yes|yeah|yep|ok|okay|oui|d'accord|go|go ahead|continue|do it|proceed|sure|lgtm|ship it|valide|validé|c'est bon|parfait|nice|great|cool|thanks|merci|thx|ty|k|👍|✅)\s*[.!]?\s*$`)

func shouldSkipFollowup(userMessage string) bool {
	text := strings.TrimSpace(userMessage)
	if len(text) > 50 {
		return false
	}
	return followupPattern.MatchString(text)
}

func shouldRunThisTurn(turn int, cfg map[string]any, userMessage string) bool {
	if truthy(classifierGet(cfg, "skip_followups")) && shouldSkipFollowup(userMessage) {
		return false
	}
	switch classifierGetStr(cfg, "frequency") {
	case "first_turn":
		return turn == 0
	case "every_n_turns":
		n := classifierGetInt(cfg, "frequency_n", 3)
		if n <= 0 {
			n = 3
		}
		return turn%n == 0
	default:
		return true
	}
}

type ClassifyInput struct {
	UserMessage   string
	Capabilities  []string
	ToolInventory []ToolInfo
	Workspace     map[string]any
	Recent        []HistMsg
}

type ToolInfo struct {
	Name        string
	Description string
}

type HistMsg struct {
	Role      string
	Content   string
	ToolCalls []HistToolCall
}

type HistToolCall struct {
	Name string
	Args string
}

func buildClassifyMessages(cfg map[string]any, in ClassifyInput, activeRules []string, profileCtx map[string]any, sessionState map[string]any) (system, user string) {
	var parts []string
	parts = append(parts, "## User message\n"+in.UserMessage)

	if classifierContextOn(cfg, "tool_inventory") && len(in.ToolInventory) > 0 {
		var lines []string
		for _, t := range in.ToolInventory {
			lines = append(lines, fmt.Sprintf("- **%s**: %s", t.Name, t.Description))
		}
		parts = append(parts, "## Agent tools\n"+strings.Join(lines, "\n"))
	} else if len(in.Capabilities) > 0 {
		parts = append(parts, "## Agent capabilities (modules)\n"+strings.Join(in.Capabilities, ", "))
	}

	if classifierContextOn(cfg, "session_state") && len(sessionState) > 0 {
		parts = append(parts, "## Session state\n"+formatSessionState(sessionState))
	} else if len(sessionState) == 0 {
		parts = append(parts, "## Session state\nTurn: 0 (fresh session)")
	}

	if classifierContextOn(cfg, "workspace_info") && len(in.Workspace) > 0 {
		if ws := formatWorkspace(in.Workspace); ws != "" {
			parts = append(parts, "## Workspace context\n"+ws)
		}
	}
	if len(activeRules) > 0 {
		parts = append(parts, "## Active behavioral rules\n"+strings.Join(activeRules, ", "))
	}
	if len(profileCtx) > 0 {
		if p := formatProfile(profileCtx); p != "" {
			parts = append(parts, "## Behavior profile\n"+p)
		}
	}
	if classifierContextOn(cfg, "recent_history") && len(in.Recent) > 0 {
		depth := 8
		if cfg != nil {
			if ctx, ok := cfg["context"].(map[string]any); ok {
				depth = intFromAny(ctx["history_depth"], 8)
			}
		}
		if hl := formatRichHistory(in.Recent, depth); len(hl) > 0 {
			parts = append(parts, "## Recent conversation\n"+strings.Join(hl, "\n"))
		}
	}

	parts = append(parts,
		"## CRITICAL - your role\n"+
			"You are the COACH. You are NOT the agent. Analyze the agent's upcoming turn and "+
			"produce directives telling it HOW to approach the task. You do NOT execute it.\n\n"+
			"## Output - RESPOND WITH JSON ONLY\n"+
			"Output a single JSON object: "+
			`{"complexity": "...", "approach": "...", "risk_level": "...", "directives": ["...", "..."]}. `+
			"No preamble, no markdown fences, no prose. Use `skip_reason` ONLY for trivial follow-ups.")

	return buildSystemPrompt(cfg), strings.Join(parts, "\n\n")
}

var scalarStateKeys = map[string]bool{
	"turn": true, "total_tool_calls": true, "tool_calls_this_turn": true,
	"consecutive_same_tool": true, "last_tool_name": true,
	"violations_count": true, "last_violation": true, "recent_tools": true,
}

func formatSessionState(state map[string]any) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Turn: %d", intFromAny(state["turn"], 0)))
	if v := intFromAny(state["total_tool_calls"], 0); v != 0 {
		lines = append(lines, fmt.Sprintf("Total tool calls: %d", v))
	}
	if v := intFromAny(state["tool_calls_this_turn"], 0); v != 0 {
		lines = append(lines, fmt.Sprintf("Tool calls this turn: %d", v))
	}
	if consec := intFromAny(state["consecutive_same_tool"], 0); consec > 2 {
		last, _ := state["last_tool_name"].(string)
		lines = append(lines, fmt.Sprintf("Repeating: %s x%d", last, consec))
	}
	if v := intFromAny(state["violations_count"], 0); v != 0 {
		last, _ := state["last_violation"].(string)
		lines = append(lines, fmt.Sprintf("Violations: %d (last: %s)", v, last))
	}

	for _, key := range sortedKeys(state) {
		if scalarStateKeys[key] || strings.HasPrefix(key, "counter:") || strings.HasPrefix(key, "flag:") {
			continue
		}
		if list, ok := state[key].([]any); ok && key != "recent_tools" {
			sample := list
			if len(list) > 5 {
				sample = list[len(list)-5:]
			}
			strs := make([]string, 0, len(sample))
			for _, s := range sample {
				strs = append(strs, fmt.Sprintf("%v", s))
			}
			lines = append(lines, fmt.Sprintf("%s: %d (%s)", titleizeKey(key), len(list), strings.Join(strs, ", ")))
		}
	}
	for _, key := range sortedKeys(state) {
		if strings.HasPrefix(key, "counter:") {
			if v := intFromAny(state[key], 0); v != 0 {
				lines = append(lines, fmt.Sprintf("%s: %d", titleizeKey(key[len("counter:"):]), v))
			}
		}
	}
	var flags []string
	for _, key := range sortedKeys(state) {
		if strings.HasPrefix(key, "flag:") && truthy(state[key]) {
			flags = append(flags, key[len("flag:"):])
		}
	}
	if len(flags) > 0 {
		lines = append(lines, "Active flags: "+strings.Join(flags, ", "))
	}
	if recent, ok := state["recent_tools"].([]string); ok && len(recent) > 0 {
		if len(recent) > 6 {
			recent = recent[len(recent)-6:]
		}
		lines = append(lines, "Recent actions: "+strings.Join(recent, " -> "))
	}
	return strings.Join(lines, "\n")
}

func formatWorkspace(ws map[string]any) string {
	var lines []string
	for _, key := range []string{"project_type", "languages", "file_count", "framework", "has_tests", "git_status", "workspace_path"} {
		val, ok := ws[key]
		if !ok || val == nil {
			continue
		}
		if list, ok := val.([]any); ok {
			strs := make([]string, 0, len(list))
			for _, v := range list {
				strs = append(strs, fmt.Sprintf("%v", v))
			}
			val = strings.Join(strs, ", ")
		}
		lines = append(lines, fmt.Sprintf("%s: %v", titleizeKey(key), val))
	}
	return strings.Join(lines, "\n")
}

func formatProfile(ctx map[string]any) string {
	var lines []string
	pname, _ := ctx["profile_name"].(string)
	pdesc, _ := ctx["profile_description"].(string)
	if pname != "" || pdesc != "" {
		s := "Profile: **" + pname + "**"
		if pdesc != "" {
			s += " - " + pdesc
		}
		lines = append(lines, s)
	}
	if cp, _ := ctx["custom_prompt"].(string); cp != "" {
		lines = append(lines, "Profile instructions:\n"+strings.TrimSpace(cp))
	}
	return strings.Join(lines, "\n")
}

func formatRichHistory(messages []HistMsg, maxMessages int) []string {
	if maxMessages <= 0 {
		maxMessages = 8
	}
	if len(messages) > maxMessages {
		messages = messages[len(messages)-maxMessages:]
	}
	var lines []string
	for _, m := range messages {
		switch m.Role {
		case "user":
			if t := truncate(m.Content, 300); strings.TrimSpace(t) != "" {
				lines = append(lines, "[user] "+t)
			}
		case "assistant":
			if t := truncate(m.Content, 200); strings.TrimSpace(t) != "" {
				lines = append(lines, "[assistant] "+t)
			}
			tcs := m.ToolCalls
			for i, tc := range tcs {
				if i >= 5 {
					lines = append(lines, fmt.Sprintf("  -> ... +%d more", len(tcs)-5))
					break
				}
				lines = append(lines, fmt.Sprintf("  -> %s(%s)", tc.Name, truncate(tc.Args, 120)))
			}
		case "tool":
			if t := truncate(m.Content, 150); strings.TrimSpace(t) != "" {
				lines = append(lines, "  <- "+t)
			}
		case "system":
			if t := truncate(m.Content, 150); strings.TrimSpace(t) != "" && strings.Contains(t, "[BEHAVIOR") {
				lines = append(lines, "[system] "+t)
			}
		}
	}
	return lines
}

func truncate(text string, maxLen int) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen-3] + "..."
}

func parseClassification(raw string) map[string]any {
	parsed := tryParseJSON(strings.TrimSpace(raw))
	if parsed == nil {
		return nil
	}
	if reason, _ := parsed["skip_reason"].(string); reason != "" {
		if d, ok := parsed["directives"].([]any); !ok || len(d) == 0 {
			return nil
		}
	}
	return parsed
}

var reJSONBlock = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

func tryParseJSON(text string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err == nil && m != nil {
		return m
	}
	if g := reJSONBlock.FindStringSubmatch(text); g != nil {
		m = nil
		if err := json.Unmarshal([]byte(g[1]), &m); err == nil && m != nil {
			return m
		}
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start != -1 && end > start {
		m = nil
		if err := json.Unmarshal([]byte(text[start:end+1]), &m); err == nil && m != nil {
			return m
		}
	}
	return nil
}

func formatDirectiveMessage(cfg map[string]any, classification map[string]any) string {
	directives, _ := classification["directives"].([]any)
	if len(directives) == 0 {
		return ""
	}
	complexity := strOr(classification["complexity"], "moderate")
	approach := strOr(classification["approach"], "direct")
	risk := strOr(classification["risk_level"], "low")
	label := approachLabel(cfg, approach)

	prefix := classifierGetStr(cfg, "directive_prefix")
	prefix = strings.NewReplacer(
		"{complexity}", complexity, "{approach}", approach,
		"{risk}", risk, "{approach_label}", label,
	).Replace(prefix)
	if prefix == "" {
		prefix = fmt.Sprintf("[BEHAVIOR DIRECTIVE - %s complexity, %s risk]", complexity, risk)
	}

	lines := []string{
		fmt.Sprintf(`<digitorn-directive type="behavior_classifier" complexity="%s" approach="%s" risk="%s">`, complexity, approach, risk),
		prefix,
		"Approach: " + label,
		"",
	}
	maxD := classifierGetInt(cfg, "max_directives", 5)
	for i, d := range directives {
		if i >= maxD {
			break
		}
		lines = append(lines, fmt.Sprintf("%d. %v", i+1, d))
	}

	riskNames := entryNames(asList(cfg, "risk_levels"))
	threshold := classifierGetStr(cfg, "high_risk_threshold")
	if threshold == "" {
		threshold = "medium"
	}
	ti, ri := indexOfStr(riskNames, threshold), indexOfStr(riskNames, risk)
	if ti >= 0 && ri >= 0 && ri >= ti {
		if warn := classifierGetStr(cfg, "high_risk_warning"); warn != "" {
			lines = append(lines, "", strings.ReplaceAll(warn, "{risk}", risk))
		}
	}
	if footer := classifierGetStr(cfg, "directive_footer"); footer != "" {
		lines = append(lines, "", footer)
	}
	lines = append(lines, "</digitorn-directive>")
	return strings.Join(lines, "\n")
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func indexOfStr(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func titleizeKey(key string) string {
	words := strings.Split(strings.ReplaceAll(key, "_", " "), " ")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
