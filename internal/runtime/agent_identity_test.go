package runtime

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func defWith(entry string, ids ...string) *schema.AppDefinition {
	d := &schema.AppDefinition{}
	for _, id := range ids {
		d.Agents = append(d.Agents, schema.Agent{ID: id})
	}
	if entry != "" {
		d.Runtime = &schema.RuntimeBlock{EntryAgent: entry}
	}
	return d
}

func TestResolveAgent_Precedence(t *testing.T) {
	def := defWith("coordinator", "worker", "coordinator", "specialist")

	// 1. explicit logical id wins.
	if a := resolveAgent(def, "specialist"); a == nil || a.ID != "specialist" {
		t.Fatalf("explicit id must select that agent, got %v", a)
	}
	// 2. no explicit id → entry_agent.
	if a := resolveAgent(def, ""); a == nil || a.ID != "coordinator" {
		t.Fatalf("empty id must select entry_agent, got %v", a)
	}
	// 3. unknown explicit id → nil (caller errors).
	if a := resolveAgent(def, "ghost"); a != nil {
		t.Errorf("unknown id must return nil, got %v", a)
	}
}

func TestResolveAgent_FallbackToFirst(t *testing.T) {
	def := defWith("", "alpha", "beta") // no entry_agent declared
	if a := resolveAgent(def, ""); a == nil || a.ID != "alpha" {
		t.Fatalf("no entry_agent must fall back to the first agent, got %v", a)
	}
}

func TestAgentRunID_RunWinsElseLogical(t *testing.T) {
	if got := agentRunID("coding#a1b2c3", "coding"); got != "coding#a1b2c3" {
		t.Errorf("run id must win, got %q", got)
	}
	if got := agentRunID("", "main"); got != "main" {
		t.Errorf("empty run id must fall back to the logical id, got %q", got)
	}
}

func TestNewAgentRunID_DistinctAndPrefixed(t *testing.T) {
	a := NewAgentRunID("coding")
	b := NewAgentRunID("coding")
	if a == b {
		t.Errorf("two run ids for the same logical agent must be distinct: %q == %q", a, b)
	}
	if !strings.HasPrefix(a, "coding#") {
		t.Errorf("run id must keep the logical id as prefix, got %q", a)
	}
	// Re-deriving from an existing run id must not nest "#".
	c := NewAgentRunID("coding#deadbeef")
	if strings.Count(c, "#") != 1 || !strings.HasPrefix(c, "coding#") {
		t.Errorf("re-derived run id must have a single logical prefix, got %q", c)
	}
}
