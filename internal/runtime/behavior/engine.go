package behavior

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

type Engine struct {
	cfg         *schema.BehaviorConfig
	profileName string
	rules       map[string]any
	ruleDefs    []ruleDef
	tk          *tracking

	classifyEnabled bool
	classifierCfg   map[string]any

	mu       sync.Mutex
	sessions map[string]*SessionState
}

func New(cfg *schema.BehaviorConfig) *Engine {
	if cfg == nil {
		return nil
	}
	e := &Engine{
		cfg:         cfg,
		profileName: cfg.Profile,
		tk:          buildTracking(cfg),
		sessions:    map[string]*SessionState{},
	}
	e.rules = resolveProfile(cfg.Profile, cfg.Rules)
	e.ruleDefs = buildRuleDefinitions(cfg, e.rules)
	e.classifyEnabled = cfg.ClassifyTurns
	e.classifierCfg = buildClassifierCfg(cfg.Classifier)
	return e
}

func (e *Engine) ClassifyEnabled() bool { return e.classifyEnabled }

type ChatFunc func(ctx context.Context, system, user string) (string, error)

func (e *Engine) Classify(ctx context.Context, sid string, in ClassifyInput, chat ChatFunc) string {
	if !e.classifyEnabled || chat == nil {
		return ""
	}
	st := e.getSession(sid)
	if !shouldRunThisTurn(st.Turn, e.classifierCfg, in.UserMessage) {
		return ""
	}
	rules := e.effectiveRules(st)
	system, user := buildClassifyMessages(
		e.classifierCfg, in, activeRuleNames(rules), profileContext(rules), st.snapshot(),
	)
	raw, err := chat(ctx, system, user)
	if err != nil || strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed := parseClassification(raw)
	if parsed == nil {
		return ""
	}
	return formatDirectiveMessage(e.classifierCfg, parsed)
}

func (e *Engine) ClassifierTimeout() int {
	return classifierGetInt(e.classifierCfg, "timeout", 15)
}

func activeRuleNames(rules map[string]any) []string {
	var out []string
	for k, v := range rules {
		if b, ok := v.(bool); ok && b {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func profileContext(rules map[string]any) map[string]any {
	ctx := map[string]any{}
	name, _ := rules["_profile_display_name"].(string)
	if name == "" {
		name, _ = rules["_profile_name"].(string)
	}
	if name != "" {
		ctx["profile_name"] = name
	}
	if d, _ := rules["_profile_description"].(string); d != "" {
		ctx["profile_description"] = d
	}
	if cp, _ := rules["_custom_prompt"].(string); cp != "" {
		ctx["custom_prompt"] = cp
	}
	return ctx
}

func buildClassifierCfg(c *schema.ClassifierConfig) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	m := map[string]any{}
	if c.Frequency != "" {
		m["frequency"] = string(c.Frequency)
	}
	if c.FrequencyN != 0 {
		m["frequency_n"] = c.FrequencyN
	}
	if c.SkipFollowups != nil {
		m["skip_followups"] = *c.SkipFollowups
	}
	if c.Timeout != 0 {
		m["timeout"] = c.Timeout
	}
	if len(c.ComplexityLevels) != 0 {
		m["complexity_levels"] = c.ComplexityLevels
	}
	if len(c.Approaches) != 0 {
		m["approaches"] = c.Approaches
	}
	if len(c.RiskLevels) != 0 {
		m["risk_levels"] = c.RiskLevels
	}
	if c.MaxDirectives != 0 {
		m["max_directives"] = c.MaxDirectives
	}
	if c.SystemPrompt != "" {
		m["system_prompt"] = c.SystemPrompt
	}
	if c.DirectivePrefix != "" {
		m["directive_prefix"] = c.DirectivePrefix
	}
	if c.HighRiskWarning != "" {
		m["high_risk_warning"] = c.HighRiskWarning
	}
	if c.HighRiskThreshold != "" {
		m["high_risk_threshold"] = c.HighRiskThreshold
	}
	if c.DirectiveFooter != "" {
		m["directive_footer"] = c.DirectiveFooter
	}
	if c.Context != nil {
		m["context"] = map[string]any{
			"tool_inventory": c.Context.ToolInventory,
			"session_state":  c.Context.SessionState,
			"workspace_info": c.Context.WorkspaceInfo,
			"recent_history": c.Context.RecentHistory,
			"history_depth":  c.Context.HistoryDepth,
		}
	}
	return m
}

func (e *Engine) getSession(sid string) *SessionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.sessions[sid]
	if st == nil {
		st = newSessionState()
		e.sessions[sid] = st
	}
	return st
}

func (e *Engine) CleanupSession(sid string) {
	e.mu.Lock()
	delete(e.sessions, sid)
	e.mu.Unlock()
}

func (e *Engine) effectiveRuleDefs(st *SessionState) []ruleDef {
	if st.ruleDefs != nil {
		return st.ruleDefs
	}
	return e.ruleDefs
}

func (e *Engine) effectiveRules(st *SessionState) map[string]any {
	if st.rules != nil {
		return st.rules
	}
	return e.rules
}

func (e *Engine) SetActiveProfile(sid, profile string) {
	st := e.getSession(sid)
	target := strings.TrimSpace(profile)
	if target == st.activeProfile {
		return
	}
	effective := target
	if effective == "" {
		effective = e.profileName
	}
	newRules := resolveProfile(effective, e.cfg.Rules)
	st.activeProfile = target
	if target == "" {
		st.rules = nil
		st.ruleDefs = nil
		return
	}
	st.rules = newRules
	st.ruleDefs = buildRuleDefinitions(e.cfg, newRules)
}

func (e *Engine) OnTurnStart(sid string) {
	e.getSession(sid).onNewTurn()
}

func (e *Engine) OnAgentText(sid, text string) []Violation {
	st := e.getSession(sid)
	if strings.TrimSpace(text) != "" {
		st.PlanStated = true
	}
	return checkRules(e.effectiveRuleDefs(st), st, "*", nil, "on_text", nil, text, e.tk)
}

func (e *Engine) PreTool(sid, tool string, params map[string]any, agentText string) []Violation {
	st := e.getSession(sid)
	vios := checkRules(e.effectiveRuleDefs(st), st, tool, params, "pre_tool", nil, agentText, e.tk)
	for _, v := range vios {
		st.ViolationsCount++
		st.LastViolation = v.RuleID
	}
	return vios
}

func (e *Engine) BlockedSubTool(sid, tool string, params map[string]any) *Violation {
	st := e.getSession(sid)
	for _, v := range checkRules(e.effectiveRuleDefs(st), st, tool, params, "pre_tool", nil, "", e.tk) {
		if v.Level == "block" {
			vv := v
			return &vv
		}
	}
	return nil
}

func (e *Engine) PostTool(sid, tool string, params map[string]any, result any) []Violation {
	st := e.getSession(sid)
	updateState(st, tool, params, e.tk)
	rem := checkRules(e.effectiveRuleDefs(st), st, tool, params, "post_tool", result, "", e.tk)
	for _, v := range rem {
		if v.Level == "warn" || v.Level == "block" {
			st.ViolationsCount++
		}
	}
	return rem
}

func (e *Engine) PromptText(sid string) string {
	st := e.getSession(sid)
	rules := e.effectiveRules(st)
	var b strings.Builder
	if section := buildPromptSection(e.effectiveRuleDefs(st)); section != "" {
		b.WriteString("## ENFORCED BEHAVIORAL RULES\n")
		b.WriteString(section)
	}
	profile, _ := rules["_profile_name"].(string)
	if profile == "" {
		profile = e.profileName
	}
	if profile == "dev" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## DEVELOPER BEHAVIOR GUIDE\n")
		b.WriteString(devPromptSection)
	}
	if cp, _ := rules["_custom_prompt"].(string); cp != "" {
		name, _ := rules["_profile_display_name"].(string)
		if name == "" {
			name = "Custom"
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## BEHAVIOR PROFILE: " + name + "\n")
		b.WriteString(cp)
	}
	return b.String()
}

func buildRuleDefinitions(cfg *schema.BehaviorConfig, merged map[string]any) []ruleDef {
	var defs []ruleDef
	seen := map[string]bool{}

	for _, rd := range cfg.RuleDefinitions {
		conv := convertExplicitRule(rd)
		if conv.id == "" || seen[conv.id] {
			continue
		}
		defs = append(defs, conv)
		seen[conv.id] = true
	}

	defaults := defaultRuleDefinitions()
	byID := make(map[string]int, len(defaults))
	for i := range defaults {
		byID[defaults[i].id] = i
	}
	for _, boolKey := range boolFlagOrder {
		ruleID := boolToRuleID[boolKey]
		if seen[ruleID] {
			continue
		}
		if !truthy(merged[boolKey]) {
			continue
		}
		idx, ok := byID[ruleID]
		if !ok {
			continue
		}
		r := defaults[idx]
		applyThresholds(&r, merged)
		defs = append(defs, r)
		seen[ruleID] = true
	}

	for _, c := range cfg.Custom {
		addLegacyCustom(&defs, seen, map[string]any(c))
	}
	if cl, ok := merged["custom"].([]any); ok {
		for _, item := range cl {
			if m, ok := item.(map[string]any); ok {
				addLegacyCustom(&defs, seen, m)
			}
		}
	}
	return defs
}

func addLegacyCustom(defs *[]ruleDef, seen map[string]bool, custom map[string]any) {
	conv := convertLegacyCustom(custom)
	if conv.id == "" || seen[conv.id] {
		return
	}
	*defs = append(*defs, conv)
	seen[conv.id] = true
}

func applyThresholds(r *ruleDef, merged map[string]any) {
	switch r.id {
	case "search_before_read":
		threshold := intFlag(merged, "max_blind_reads", 3)
		if all, ok := r.condition["all"].([]any); ok {
			for _, c := range all {
				if m, ok := c.(map[string]any); ok {
					if cg, ok := m["counter_gte"].(map[string]any); ok {
						cg["value"] = threshold
					}
				}
			}
		}
	case "test_after_changes":
		threshold := intFlag(merged, "changes_before_test_reminder", 3)
		if cg, ok := r.condition["counter_gte"].(map[string]any); ok {
			cg["value"] = threshold
		}
	case "max_sequential_same_tool":
		r.condition["consecutive_gte"] = intFlag(merged, "max_sequential_same_tool", 8)
	}
}

func convertExplicitRule(rd schema.BehaviorRuleDefinition) ruleDef {
	when := string(rd.When)
	if when == "" {
		when = "pre_tool"
	}
	action := string(rd.Action)
	if action == "" {
		action = "warn"
	}
	cond := rd.Condition
	if cond == nil {
		cond = map[string]any{}
	}
	msg := rd.Message
	if msg == "" {
		msg = rd.Description
	}
	return ruleDef{
		id:          rd.ID,
		description: rd.Description,
		when:        when,
		action:      action,
		message:     msg,
		trigger:     toTriggerList(rd.Trigger),
		condition:   cond,
	}
}

func convertLegacyCustom(custom map[string]any) ruleDef {
	trigger, _ := custom["trigger"].(string)
	cond := map[string]any{}
	if old, ok := custom["condition"].(map[string]any); ok {
		pname, _ := old["param"].(string)
		if v, ok := old["contains"]; ok {
			cond = map[string]any{"param_contains": map[string]any{"param": pname, "value": v}}
		} else if v, ok := old["matches"]; ok {
			cond = map[string]any{"param_matches": map[string]any{"param": pname, "pattern": v}}
		} else if v, ok := old["not_in"]; ok {
			cond = map[string]any{"target_not_in_set": v}
		}
	}
	id, _ := custom["id"].(string)
	if id == "" {
		id = "custom"
	}
	rule, _ := custom["rule"].(string)
	when, _ := custom["enforce"].(string)
	if when == "" {
		when = "pre_tool"
	}
	action, _ := custom["action"].(string)
	if action == "" {
		action = "warn"
	}
	msg, _ := custom["message"].(string)
	if msg == "" {
		msg = rule
	}
	var trig []string
	if trigger != "" {
		trig = []string{trigger}
	}
	return ruleDef{
		id:          id,
		description: rule,
		when:        when,
		action:      action,
		message:     msg,
		trigger:     trig,
		condition:   cond,
	}
}

func toTriggerList(raw any) []string {
	switch v := raw.(type) {
	case string:
		if v == "*" || v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func buildTracking(cfg *schema.BehaviorConfig) *tracking {
	st := cfg.StateTracking
	if st == nil {
		return defaultStateTracking
	}
	tk := &tracking{
		sets:     map[string]setCfg{},
		counters: map[string]counterCfg{},
		flags:    map[string]flagCfg{},
	}
	for name, c := range st.Sets {
		tk.sets[name] = setCfg{addOn: c.AddOn, target: c.Target, aliases: c.Aliases}
	}
	for name, c := range st.Counters {
		cc := counterCfg{incrementOn: c.IncrementOn, resetOn: c.ResetOn}
		if len(c.ResetWhen) > 0 {
			cc.resetWhen = &resetWhen{
				tool:    c.ResetWhen["tool"],
				param:   c.ResetWhen["param"],
				matches: c.ResetWhen["matches"],
			}
		}
		tk.counters[name] = cc
	}
	for name, c := range st.Flags {
		tk.flags[name] = flagCfg{setOn: c.SetOn, unsetOn: c.UnsetOn}
	}
	return tk
}
