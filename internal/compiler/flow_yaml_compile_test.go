package compiler_test

import (
	"os"
	"path/filepath"
	"testing"
)

const flowAppYAML = `schema_version: 2
app:
  app_id: support-flow
  name: Support Flow
  version: "1.0"
  category: "coding"

agents:
  - id: triage
    role: worker
    brain:
      provider: openai
      model: gpt-4o-mini
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "Classify the request as refund, tech, or other. Output JSON."
  - id: refunder
    role: worker
    brain:
      provider: openai
      model: gpt-4o-mini
      config:
        api_key: "{{env.OPENAI_API_KEY}}"
    system_prompt: "Handle the refund."

tools:
  modules:
    filesystem: {}
  capabilities:
    default_policy: auto
    max_risk_level: high

flow:
  id: support
  entry: triage_node
  max_iterations: 50
  description: "Triage with a refund path, a tool write, and an end sentinel."
  nodes:
    - id: triage_node
      type: agent
      agent: triage
      params:
        task: "{{event.payload.message}}"
      routes:
        - { when: "category == 'refund'", to: refund_node }
        - { when: "category == 'tech'", to: tech_node }
        - { default: true, to: end }

    - id: refund_node
      type: agent
      agent: refunder
      params:
        task: "Process refund for: {{event.text}}"
      routes:
        - { to: write_node }

    - id: write_node
      type: tool
      tool: filesystem.write
      params:
        path: "refund.txt"
        content: "done"
      on_error:
        - { match: "denied", to: notify_node }
        - { default: true, to: end }
      routes:
        - { to: end }

    - id: tech_node
      type: agent
      agent: triage
      routes:
        - { to: end }

    - id: notify_node
      type: terminal

    - id: decide
      type: decision
      expr: "priority"
      routes:
        - { when: "p0", to: notify_node }
        - { default: true, to: end }
`

func TestFlowYAML_CompilesCleanWithEndSentinel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(flowAppYAML), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}

	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !res.OK() {
		t.Fatalf("flow app with `end` sentinel must compile clean; diagnostics:\n%v", res.Diagnostics)
	}

	def := res.Definition
	if def.Flow == nil {
		t.Fatal("compiled definition has no flow")
	}
	if def.Flow.Entry != "triage_node" {
		t.Errorf("flow entry = %q, want triage_node", def.Flow.Entry)
	}
	if len(def.Flow.Nodes) != 6 {
		t.Errorf("flow nodes = %d, want 6", len(def.Flow.Nodes))
	}

	var triage *int
	for i := range def.Flow.Nodes {
		if def.Flow.Nodes[i].ID == "triage_node" {
			triage = &i
		}
	}
	if triage == nil {
		t.Fatal("triage_node missing after compile")
	}
	routes := def.Flow.Nodes[*triage].Routes
	if len(routes) != 3 {
		t.Fatalf("triage routes = %d, want 3", len(routes))
	}
	if routes[2].To != "end" {
		t.Errorf("default route should target end sentinel, got %q", routes[2].To)
	}
	if routes[0].When != "category == 'refund'" {
		t.Errorf("first route when = %q", routes[0].When)
	}
}

func TestFlowYAML_AcceptsFlowContextRefs(t *testing.T) {
	yaml := `schema_version: 2
app: { app_id: ctx-flow, name: Ctx, version: "1.0", category: "coding" }
agents:
  - id: a
    role: worker
    brain: { provider: openai, model: gpt-4o-mini, config: { api_key: "k" } }
    system_prompt: "x"
flow:
  id: f
  entry: classify
  nodes:
    - id: classify
      type: agent
      agent: a
      params:
        task: "{{event.payload.message}}"
      routes:
        - { when: "category == 'refund'", to: gate }
        - { default: true, to: end }
    - id: gate
      type: approval
      message: "Refund {{classify.output.amount}} — approve?"
      routes:
        - { when: "approvals.gate == 'approve'", to: done }
        - { default: true, to: end }
    - id: done
      type: terminal
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := newCompilerForFixtures(t).Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !res.OK() {
		t.Fatalf("flow with approvals.* and {{node.output.x}} must compile clean; diagnostics:\n%v", res.Diagnostics)
	}
}

func TestFlowYAML_RejectsUnknownAgent(t *testing.T) {
	bad := `schema_version: 2
app: { app_id: bad-agent, name: Bad, version: "1.0", category: "coding" }
agents:
  - id: real
    role: worker
    brain: { provider: openai, model: gpt-4o-mini, config: { api_key: "k" } }
    system_prompt: "x"
flow:
  id: f
  entry: n1
  nodes:
    - id: n1
      type: agent
      agent: ghost
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := newCompilerForFixtures(t).Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.OK() {
		t.Fatal("flow node referencing an undeclared agent must NOT compile clean")
	}
}

func TestFlowYAML_RejectsCycleWithoutCap(t *testing.T) {
	bad := `schema_version: 2
app: { app_id: cyc, name: Cyc, version: "1.0", category: "coding" }
agents:
  - id: a
    role: worker
    brain: { provider: openai, model: gpt-4o-mini, config: { api_key: "k" } }
    system_prompt: "x"
flow:
  id: f
  entry: n1
  nodes:
    - id: n1
      type: agent
      agent: a
      routes:
        - { to: n2 }
    - id: n2
      type: agent
      agent: a
      routes:
        - { to: n1 }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := newCompilerForFixtures(t).Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.OK() {
		t.Fatal("a cyclic flow without max_iterations must NOT compile clean")
	}
}

func TestFlowYAML_CycleAllowedWithCap(t *testing.T) {
	ok := `schema_version: 2
app: { app_id: cyc2, name: Cyc2, version: "1.0", category: "coding" }
agents:
  - id: a
    role: worker
    brain: { provider: openai, model: gpt-4o-mini, config: { api_key: "k" } }
    system_prompt: "x"
flow:
  id: f
  entry: n1
  max_iterations: 10
  nodes:
    - id: n1
      type: agent
      agent: a
      routes:
        - { when: "done == 'yes'", to: end }
        - { default: true, to: n1 }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(ok), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := newCompilerForFixtures(t).Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !res.OK() {
		t.Fatalf("a self-loop WITH max_iterations must compile clean; diagnostics:\n%v", res.Diagnostics)
	}
}

func TestFlowYAML_RejectsDanglingRoute(t *testing.T) {
	bad := `schema_version: 2
app: { app_id: bad-flow, name: Bad, version: "1.0", category: "coding" }
agents:
  - id: a
    role: worker
    brain: { provider: openai, model: gpt-4o-mini, config: { api_key: "k" } }
    system_prompt: "x"
flow:
  id: bad
  entry: n1
  nodes:
    - id: n1
      type: agent
      agent: a
      routes:
        - { to: nonexistent_node }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := newCompilerForFixtures(t)
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.OK() {
		t.Fatal("flow routing to an undeclared node must NOT compile clean")
	}
}
