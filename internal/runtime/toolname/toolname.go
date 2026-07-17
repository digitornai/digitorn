package toolname

import "strings"

func Canonicalize(name string) string {
	if name == "" {
		return name
	}
	if strings.Contains(name, ".") {
		return name
	}
	if idx := strings.Index(name, "::"); idx != -1 {
		return name[:idx] + "." + name[idx+2:]
	}
	idx := strings.Index(name, "__")
	if idx == -1 {
		return name
	}
	return name[:idx] + "." + name[idx+2:]
}

func Sanitize(name string) string {
	if name == "" {
		return name
	}
	idx := strings.Index(name, ".")
	if idx == -1 {
		return name
	}
	return name[:idx] + "__" + name[idx+1:]
}

var internalAliases = map[string]string{
	"agent":       "agent_spawn.agent",
	"Agent":       "agent_spawn.agent",
	"remember":    "memory.remember",
	"Remember":    "memory.remember",
	"set_goal":    "memory.set_goal",
	"SetGoal":     "memory.set_goal",
	"task_create": "memory.task_create",
	"TaskCreate":  "memory.task_create",
	"task_update": "memory.task_update",
	"TaskUpdate":  "memory.task_update",
	"search_tools":    "context_builder.search_tools",
	"get_tool":        "context_builder.get_tool",
	"execute_tool":    "context_builder.execute_tool",
	"list_categories": "context_builder.list_categories",
	"browse_category": "context_builder.browse_category",
	"run_parallel":    "context_builder.run_parallel",
	"background_run":  "context_builder.background_run",
	"use_skill":       "context_builder.use_skill",
	"call_app":        "context_builder.call_app",
	"ask_user":        "context_builder.ask_user",
}

var internalFQNByFlat = func() map[string]string {
	m := make(map[string]string, len(internalAliases))
	for _, fqn := range internalAliases {
		m[flattenKey(fqn)] = fqn
	}
	return m
}()

func ResolveAlias(name string) string {
	if fqn, ok := internalAliases[name]; ok {
		return fqn
	}
	if !strings.Contains(name, ".") {
		if fqn, ok := internalFQNByFlat[flattenKey(name)]; ok {
			return fqn
		}
	}
	return name
}

func SplitFQN(name string) (module, action string) {
	canonical := Canonicalize(name)
	idx := strings.Index(canonical, ".")
	if idx == -1 {
		return "", canonical
	}
	return canonical[:idx], canonical[idx+1:]
}

func QualifyBareName(name string, knownFQNs []string) string {
	if name == "" || strings.Contains(name, ".") {
		return name
	}
	match := ""
	for _, fqn := range knownFQNs {
		dot := strings.IndexByte(fqn, '.')
		if dot < 0 || fqn[dot+1:] != name {
			continue
		}
		if match != "" && match != fqn {
			return name
		}
		match = fqn
	}
	if match != "" {
		return match
	}
	return name
}

func flattenKey(s string) string {
	s = strings.ReplaceAll(s, "__", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

func ResolveMangled(name string, knownFQNs []string) string {
	if name == "" || strings.Contains(name, ".") {
		return name
	}
	key := flattenKey(name)
	match := ""
	for _, fqn := range knownFQNs {
		if flattenKey(fqn) != key {
			continue
		}
		if match != "" && match != fqn {
			return name
		}
		match = fqn
	}
	if match != "" {
		return match
	}
	return name
}
