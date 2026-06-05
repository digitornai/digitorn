package validate

import (
	"fmt"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
)

func (v *validator) checkCycles() {
	v.checkDelegateCycles()
	v.checkFlowCycles()
}

func (v *validator) checkDelegateCycles() {
	graph := make(map[string][]string, len(v.def.Agents))
	for _, a := range v.def.Agents {
		graph[a.ID] = a.DelegateTo
		if a.Coordination != nil {
			graph[a.ID] = append(graph[a.ID], a.Coordination.DelegateTo...)
		}
	}
	for id := range graph {
		if cycle := findCycle(graph, id, map[string]bool{}, []string{}); cycle != nil {
			v.errf(diagnostic.CodeCycleDelegate, "agents",
				"delegate_to cycle: %s", strings.Join(cycle, " -> "))
			return
		}
	}
}

func (v *validator) checkFlowCycles() {
	if v.def.Flow == nil || len(v.def.Flow.Nodes) == 0 {
		return
	}
	graph := make(map[string][]string, len(v.def.Flow.Nodes))
	allowsLoop := make(map[string]bool)
	for _, n := range v.def.Flow.Nodes {
		for _, r := range n.Routes {
			graph[n.ID] = append(graph[n.ID], r.To)
		}
		if n.MaxIters > 0 {
			allowsLoop[n.ID] = true
		}
	}
	for id := range graph {
		if cycle := findCycle(graph, id, map[string]bool{}, []string{}); cycle != nil {
			if hasAllowedLoop(cycle, allowsLoop) {
				continue
			}
			v.errf(diagnostic.CodeCycleFlow, "flow.nodes",
				"flow cycle: %s (set max_iterations on a node to allow looping)",
				strings.Join(cycle, " -> "))
			return
		}
	}
}

func hasAllowedLoop(cycle []string, allowed map[string]bool) bool {
	for _, id := range cycle {
		if allowed[id] {
			return true
		}
	}
	return false
}

func findCycle(graph map[string][]string, start string, visiting map[string]bool, path []string) []string {
	if visiting[start] {
		return append(path, start)
	}
	visiting[start] = true
	defer delete(visiting, start)
	path = append(path, start)
	for _, next := range graph[start] {
		if c := findCycle(graph, next, visiting, path); c != nil {
			return c
		}
	}
	return nil
}

var _ = fmt.Sprintf
