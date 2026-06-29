// Package refs validates every named reference in an AppDefinition against
// the built-in catalog and reports unknown identifiers with did-you-mean.
package refs

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/parse"
	"github.com/mbathepaul/digitorn/internal/compiler/position"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/compiler/suggest"
)

type checker struct {
	file string
	doc  *yaml.Node
	cat  *catalog.Catalog
	bag  *diagnostic.Bag
	def  *schema.AppDefinition
	ids  map[string]struct{}
}

func Check(file string, doc *yaml.Node, def *schema.AppDefinition, cat *catalog.Catalog, bag *diagnostic.Bag) {
	c := &checker{file: file, doc: doc, cat: cat, bag: bag, def: def, ids: agentIDs(def)}
	c.checkAgents()
	c.checkTools()
	c.checkRuntime()
	c.checkFlow()
}

func (c *checker) pos(path string) position.Pos { return parse.LookupPos(c.file, c.doc, path) }

func (c *checker) emit(code diagnostic.Code, pos position.Pos, msg string, suggestion string) {
	d := diagnostic.Errorf(code, pos, "%s", msg)
	if suggestion != "" {
		d = d.WithSuggestion(suggestion, fmt.Sprintf("did you mean %q?", suggestion))
	}
	c.bag.Add(d)
}

func (c *checker) warn(code diagnostic.Code, pos position.Pos, msg string) {
	c.bag.Add(diagnostic.Warningf(code, pos, "%s", msg))
}

func (c *checker) checkAgents() {
	for i, a := range c.def.Agents {
		base := fmt.Sprintf("agents.%d", i)
		c.checkBrain(a.Brain, base+".brain")
		for j, target := range a.DelegateTo {
			if _, ok := c.ids[target]; !ok {
				s, _ := suggest.Closest(target, mapKeys(c.ids), 2)
				c.emit(diagnostic.CodeUnknownAgent, c.pos(fmt.Sprintf("%s.delegate_to.%d", base, j)),
					fmt.Sprintf("unknown agent %q in delegate_to", target), s)
			}
		}
		for j, m := range a.Modules {
			if !c.cat.HasModule(m.ID) {
				s, _ := suggest.Closest(m.ID, c.cat.ModuleIDs(), 2)
				c.emit(diagnostic.CodeUnknownModule, c.pos(fmt.Sprintf("%s.modules.%d", base, j)),
					fmt.Sprintf("unknown module %q", m.ID), s)
				continue
			}
			for _, tool := range m.Tools {
				if !c.cat.HasTool(m.ID, tool) {
					s, _ := suggest.Closest(tool, c.cat.ToolsFor(m.ID), 2)
					c.emit(diagnostic.CodeUnknownTool, c.pos(fmt.Sprintf("%s.modules.%d", base, j)),
						fmt.Sprintf("module %q has no tool %q", m.ID, tool), s)
				}
			}
		}
		for j, h := range a.Hooks {
			c.checkHook(h, fmt.Sprintf("%s.hooks.%d", base, j))
		}
	}
}

func (c *checker) checkBrain(b schema.Brain, base string) {
	if b.Provider != "" && !c.cat.HasProvider(b.Provider) {
		s, _ := suggest.Closest(b.Provider, c.cat.Providers(), 3)
		c.emit(diagnostic.CodeUnknownProvider, c.pos(base+".provider"),
			fmt.Sprintf("unknown provider %q", b.Provider), s)
	}
	if b.Fallback != nil {
		c.checkBrain(*b.Fallback, base+".fallback")
	}
}

func (c *checker) checkHook(h schema.Hook, base string) {
	if h.On != "" && !isEnum(string(h.On), hookEventStrings()) {
		s, _ := suggest.Closest(string(h.On), hookEventStrings(), 3)
		c.emit(diagnostic.CodeUnknownHookEvent, c.pos(base+".on"),
			fmt.Sprintf("unknown hook event %q", h.On), s)
	}
	// Lint : a VALID but not-yet-routed event compiles, yet the runtime
	// never emits it — warn so the hook isn't silently dead.
	for _, ev := range []schema.HookEvent{h.On, h.Event} {
		if ev == "" {
			continue
		}
		if reason, dead := schema.NotYetRoutedHookEvents[ev]; dead {
			c.warn(diagnostic.CodeHookEventNotRouted, c.pos(base+".on"),
				fmt.Sprintf("hook event %q is not emitted by this runtime build (%s) — the hook will not fire", ev, reason))
		}
	}
	if h.Condition.Type != "" && !isEnum(string(h.Condition.Type), hookConditionStrings()) {
		s, _ := suggest.Closest(string(h.Condition.Type), hookConditionStrings(), 3)
		c.emit(diagnostic.CodeUnknownHookCondition, c.pos(base+".condition.type"),
			fmt.Sprintf("unknown hook condition %q", h.Condition.Type), s)
	}
	if h.Action.Type != "" && !isEnum(string(h.Action.Type), hookActionStrings()) {
		s, _ := suggest.Closest(string(h.Action.Type), hookActionStrings(), 3)
		c.emit(diagnostic.CodeUnknownHookAction, c.pos(base+".action.type"),
			fmt.Sprintf("unknown hook action %q", h.Action.Type), s)
	}
}

func (c *checker) checkTools() {
	if c.def.Tools == nil {
		return
	}
	for id := range c.def.Tools.Modules {
		if !c.cat.HasModule(id) {
			s, _ := suggest.Closest(id, c.cat.ModuleIDs(), 2)
			c.emit(diagnostic.CodeUnknownModule, c.pos("tools.modules."+id),
				fmt.Sprintf("unknown module %q", id), s)
		}
	}
	if c.def.Tools.Capabilities != nil {
		c.checkGrants(c.def.Tools.Capabilities.Grant, "tools.capabilities.grant")
		c.checkGrants(c.def.Tools.Capabilities.Approve, "tools.capabilities.approve")
		c.checkGrants(c.def.Tools.Capabilities.Deny, "tools.capabilities.deny")
		c.checkGrants(c.def.Tools.Capabilities.HiddenActions, "tools.capabilities.hidden_actions")
		for j, mod := range c.def.Tools.Capabilities.HiddenModules {
			if !c.cat.HasModule(mod) {
				s, _ := suggest.Closest(mod, c.cat.ModuleIDs(), 2)
				c.emit(diagnostic.CodeUnknownModule,
					c.pos(fmt.Sprintf("tools.capabilities.hidden_modules.%d", j)),
					fmt.Sprintf("unknown module %q", mod), s)
			}
		}
	}
	for id, ch := range c.def.Tools.Channels {
		if !c.cat.HasChannelType(ch.Type) {
			s, _ := suggest.Closest(ch.Type, c.cat.ChannelTypes(), 2)
			c.emit(diagnostic.CodeUnknownChannelType,
				c.pos("tools.channels."+id+".type"),
				fmt.Sprintf("unknown channel type %q", ch.Type), s)
		}
		if ch.UserResolver != nil {
			if !c.cat.HasModule(ch.UserResolver.Module) {
				s, _ := suggest.Closest(ch.UserResolver.Module, c.cat.ModuleIDs(), 2)
				c.emit(diagnostic.CodeUnknownModule,
					c.pos("tools.channels."+id+".user_resolver.module"),
					fmt.Sprintf("unknown module %q", ch.UserResolver.Module), s)
			} else if !c.cat.HasTool(ch.UserResolver.Module, ch.UserResolver.Action) {
				s, _ := suggest.Closest(ch.UserResolver.Action, c.cat.ToolsFor(ch.UserResolver.Module), 2)
				c.emit(diagnostic.CodeUnknownTool,
					c.pos("tools.channels."+id+".user_resolver.action"),
					fmt.Sprintf("module %q has no tool %q", ch.UserResolver.Module, ch.UserResolver.Action), s)
			}
		}
	}
}

func (c *checker) checkGrants(grants []schema.CapabilityGrant, base string) {
	for i, g := range grants {
		if !c.cat.HasModule(g.Module) {
			s, _ := suggest.Closest(g.Module, c.cat.ModuleIDs(), 2)
			c.emit(diagnostic.CodeUnknownModule,
				c.pos(fmt.Sprintf("%s.%d.module", base, i)),
				fmt.Sprintf("unknown module %q", g.Module), s)
			continue
		}
		for _, tool := range g.EffectiveTools() {
			if !c.cat.HasTool(g.Module, tool) {
				s, _ := suggest.Closest(tool, c.cat.ToolsFor(g.Module), 2)
				c.emit(diagnostic.CodeUnknownTool,
					c.pos(fmt.Sprintf("%s.%d", base, i)),
					fmt.Sprintf("module %q has no tool %q", g.Module, tool), s)
			}
		}
	}
}

func (c *checker) checkRuntime() {
	if c.def.Runtime == nil {
		return
	}
	if id := c.def.Runtime.EntryAgent; id != "" {
		if _, ok := c.ids[id]; !ok {
			s, _ := suggest.Closest(id, mapKeys(c.ids), 2)
			c.emit(diagnostic.CodeUnknownAgent, c.pos("runtime.entry_agent"),
				fmt.Sprintf("unknown entry_agent %q", id), s)
		}
	}
	for i, mw := range c.def.Runtime.Middleware {
		// "custom" is the reserved plugin-transport keyword (resolved at
		// runtime via the gRPC worker), not a catalog middleware ; its
		// module/kind config is validated by validate.CheckAppMiddleware.
		if mw.Name == "custom" {
			continue
		}
		if !c.cat.HasMiddleware(mw.Name) {
			s, _ := suggest.Closest(mw.Name, c.cat.MiddlewareNames(), 2)
			c.emit(diagnostic.CodeUnknownMiddleware,
				c.pos(fmt.Sprintf("runtime.middleware.%d.name", i)),
				fmt.Sprintf("unknown middleware %q", mw.Name), s)
		}
	}
	for i, h := range c.def.Runtime.Hooks {
		c.checkHook(h, fmt.Sprintf("runtime.hooks.%d", i))
	}
	for i, t := range c.def.Runtime.Triggers {
		if t.Type != "" && !isEnum(string(t.Type), triggerTypeStrings()) {
			s, _ := suggest.Closest(string(t.Type), triggerTypeStrings(), 2)
			c.emit(diagnostic.CodeUnknownTriggerType,
				c.pos(fmt.Sprintf("runtime.triggers.%d.type", i)),
				fmt.Sprintf("unknown trigger type %q", t.Type), s)
		}
	}
}

func (c *checker) checkFlow() {
	if c.def.Flow == nil {
		return
	}
	if id := c.def.Flow.Entry; id != "" {
		if !c.hasFlowNode(id) {
			c.emit(diagnostic.CodeUnknownAgent, c.pos("flow.entry"),
				fmt.Sprintf("unknown flow node %q in flow.entry", id), "")
		}
	}
	for i, node := range c.def.Flow.Nodes {
		base := fmt.Sprintf("flow.nodes.%d", i)
		if node.Agent != "" {
			if _, ok := c.ids[node.Agent]; !ok {
				s, _ := suggest.Closest(node.Agent, mapKeys(c.ids), 2)
				c.emit(diagnostic.CodeUnknownAgent, c.pos(base+".agent"),
					fmt.Sprintf("unknown agent %q in flow node", node.Agent), s)
			}
		}
		for j, r := range node.Routes {
			if r.To != "" && !c.isFlowTarget(r.To) {
				c.emit(diagnostic.CodeUnknownAgent, c.pos(fmt.Sprintf("%s.routes.%d.to", base, j)),
					fmt.Sprintf("flow route points to unknown node %q", r.To), "")
			}
		}
		for j, r := range node.OnError {
			if r.To != "" && !c.isFlowTarget(r.To) {
				c.emit(diagnostic.CodeUnknownAgent, c.pos(fmt.Sprintf("%s.on_error.%d.to", base, j)),
					fmt.Sprintf("flow on_error route points to unknown node %q", r.To), "")
			}
		}
		for j, b := range node.Branches {
			if b.To != "" && !c.isFlowTarget(b.To) {
				c.emit(diagnostic.CodeUnknownAgent, c.pos(fmt.Sprintf("%s.branches.%d.to", base, j)),
					fmt.Sprintf("flow branch points to unknown node %q", b.To), "")
			}
		}
	}
}

// isFlowTarget reports whether a route/branch target is valid: a declared node
// or the literal "end" sentinel that terminates a path (per docs/language/07-flows).
func (c *checker) isFlowTarget(id string) bool {
	return id == "end" || c.hasFlowNode(id)
}

func (c *checker) hasFlowNode(id string) bool {
	for _, n := range c.def.Flow.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func agentIDs(def *schema.AppDefinition) map[string]struct{} {
	out := make(map[string]struct{}, len(def.Agents))
	for _, a := range def.Agents {
		if a.ID != "" {
			out[a.ID] = struct{}{}
		}
	}
	return out
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func isEnum(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func hookEventStrings() []string {
	out := make([]string, len(schema.AllHookEvents))
	for i, e := range schema.AllHookEvents {
		out[i] = string(e)
	}
	return out
}

func hookConditionStrings() []string {
	out := make([]string, len(schema.AllHookConditions))
	for i, e := range schema.AllHookConditions {
		out[i] = string(e)
	}
	return out
}

func hookActionStrings() []string {
	out := make([]string, len(schema.AllHookActions))
	for i, e := range schema.AllHookActions {
		out[i] = string(e)
	}
	return out
}

func triggerTypeStrings() []string {
	out := make([]string, len(schema.AllTriggerTypes))
	for i, t := range schema.AllTriggerTypes {
		out[i] = string(t)
	}
	return out
}
