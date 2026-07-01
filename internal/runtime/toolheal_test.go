package runtime

import (
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func globSpec() *tool.Spec {
	return &tool.Spec{
		Name: "glob",
		Params: []tool.ParamSpec{
			{Name: "pattern", Type: "string", Required: true},
			{Name: "type", Type: "string"},
			{Name: "max_results", Type: "integer"},
		},
	}
}

func TestHealToolArgs_ToolNameAlias(t *testing.T) {
	args := map[string]any{"glob": "**/*.go"}
	if !healToolArgs(globSpec(), "filesystem.glob", args) {
		t.Fatal("expected heal to fire")
	}
	if args["pattern"] != "**/*.go" {
		t.Fatalf("pattern not healed from tool-name key: %v", args)
	}
	if _, ok := args["glob"]; ok {
		t.Fatalf("misused alias key should be removed: %v", args)
	}
}

func TestHealToolArgs_NeverOverwritesProvided(t *testing.T) {
	args := map[string]any{"pattern": "*.ts", "glob": "**/*"}
	healToolArgs(globSpec(), "filesystem.glob", args)
	if args["pattern"] != "*.ts" {
		t.Fatalf("a supplied required value must win, got %v", args)
	}
}

func TestHealToolArgs_OneUnambiguousStray(t *testing.T) {
	// Model used a wrong-but-unique key that is neither the param nor the tool name.
	args := map[string]any{"query": "*.md"}
	if !healToolArgs(globSpec(), "filesystem.glob", args) {
		t.Fatal("expected one-stray heal to fire")
	}
	if args["pattern"] != "*.md" {
		t.Fatalf("pattern not healed from the lone stray key: %v", args)
	}
}

func TestHealToolArgs_AmbiguousStrayLeftAlone(t *testing.T) {
	// Two stray keys → ambiguous → do nothing (let the tool's own validation speak).
	args := map[string]any{"a": "x", "b": "y"}
	if healToolArgs(globSpec(), "filesystem.glob", args) {
		t.Fatalf("ambiguous strays must NOT be remapped: %v", args)
	}
	if _, ok := args["pattern"]; ok {
		t.Fatalf("pattern must stay missing under ambiguity: %v", args)
	}
}

func TestHealToolArgs_EmptyStillEmpty(t *testing.T) {
	// A genuinely empty call is left empty — the tool still errors, as before.
	args := map[string]any{}
	if healToolArgs(globSpec(), "filesystem.glob", args) {
		t.Fatal("empty call must not be healed into anything")
	}
}

func TestHealToolArgs_NonStringRequiredUntouched(t *testing.T) {
	spec := &tool.Spec{Name: "x", Params: []tool.ParamSpec{{Name: "count", Type: "integer", Required: true}}}
	args := map[string]any{"x": "5"} // tool-name key, but param is integer
	if healToolArgs(spec, "mod.x", args) {
		t.Fatalf("non-string required params must not be string-healed: %v", args)
	}
}
