//go:build live

package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler"
	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	_ "github.com/mbathepaul/digitorn/internal/modules/filesystem"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// compileFlowYAML compiles an inline app.yaml through the real compiler and
// returns its validated Definition. Fails the test on any diagnostic error.
func compileFlowYAML(t *testing.T, yaml string) *schema.AppDefinition {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write app.yaml: %v", err)
	}
	c := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})
	res, err := c.Compile(dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !res.OK() {
		t.Fatalf("YAML must compile clean; diagnostics:\n%v", res.Diagnostics)
	}
	return res.Definition
}

// TestLiveFlowYAML_EndToEnd compiles a real app.yaml with a flow (agent
// classifier + tool write + end sentinel), then runs it against a live LLM.
// Proves the full pipeline: YAML -> compiler -> schema -> runtime -> real model,
// including a real tool node writing to the workspace.
func TestLiveFlowYAML_EndToEnd(t *testing.T) {
	const yaml = `schema_version: 2
app:
  app_id: live-flow-yaml
  name: Live Flow YAML
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
    system_prompt: "Classify the user request as refund, tech, or other."

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
  nodes:
    - id: triage_node
      type: agent
      agent: triage
      params:
        task: "Classify and respond with ONLY JSON {\"category\":\"<refund|tech|other>\"}. Request: {{event.payload.message}}"
      routes:
        - { when: "category == 'refund'", to: write_node }
        - { default: true, to: end }

    - id: write_node
      type: tool
      tool: filesystem.write
      params:
        path: "refund_marker.txt"
        content: "refund requested"
      on_error:
        - { default: true, to: end }
      routes:
        - { to: end }
`

	def := compileFlowYAML(t, yaml)

	f := liveSetup(t)
	// Swap the fixture app to the compiled YAML definition, but keep the live
	// Brain (provider/model) the gateway actually serves.
	liveBrain := f.app.Definition.Agents[0].Brain
	for i := range def.Agents {
		def.Agents[i].Brain = liveBrain
	}
	f.app.Definition = def

	f.injectUser(t, "I want my money back for a defective item")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := f.engine.Run(ctx, dgruntime.TurnInput{
		AppID: "live-app", SessionID: "live-sess", UserID: "test-user", UserJWT: f.userJWT,
	}); err != nil {
		t.Fatalf("flow run: %v", err)
	}

	if !flowNodeRan(f, "write_node") {
		t.Error("expected the tool write_node to run for a refund request")
		dumpFlowNodes(t, f)
	}
	marker := filepath.Join(f.workspace, "refund_marker.txt")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected the flow tool node to write %s: %v", marker, err)
	}
	if string(data) != "refund requested" {
		t.Errorf("tool wrote unexpected content: %q", string(data))
	}
}
