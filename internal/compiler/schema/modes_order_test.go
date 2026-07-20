package schema_test

import (
	"encoding/json"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// ModesOrder carried `json:"-"`, so the compiler captured the YAML order and
// then dropped it when writing the compiled app (stored as JSON). On reload the
// order was rebuilt by ranging the Modes map, which Go randomizes — the mode
// picker listed modes in a different order between daemon restarts, and for an
// app without an `auto` mode the DEFAULT mode changed too ("first declared").
func TestRuntimeBlock_ModesOrderSurvivesJSON(t *testing.T) {
	in := &schema.RuntimeBlock{
		Modes: map[string]schema.ModeDef{
			"plan":  {Label: "Plan"},
			"build": {Label: "Build"},
		},
		ModesOrder:  []string{"plan", "build"},
		DefaultMode: "plan",
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out schema.RuntimeBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(out.ModesOrder) != 2 || out.ModesOrder[0] != "plan" || out.ModesOrder[1] != "build" {
		t.Fatalf("ModesOrder lost in the round-trip: got %v, want [plan build]", out.ModesOrder)
	}
	if out.DefaultMode != "plan" {
		t.Errorf("DefaultMode = %q, want plan", out.DefaultMode)
	}
}

// ModesOrder is derived from the YAML document, never authored: it must stay
// out of the hand-written surface so a manifest cannot set it directly.
func TestRuntimeBlock_ModesOrderNotAuthorableInYAML(t *testing.T) {
	raw, err := json.Marshal(&schema.RuntimeBlock{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty: an app with no modes must not carry the key at all.
	if got := string(raw); got != "{}" {
		t.Errorf("empty runtime block should marshal to {}, got %s", got)
	}
}
