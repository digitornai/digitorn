package compiler

import (
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/bundle"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func TestMergeTemplatesFragment(t *testing.T) {
	root, err := filepath.Abs("../appmgr/builtins/digitorn-craft")
	if err != nil {
		t.Fatal(err)
	}
	def := &schema.AppDefinition{}
	mergeTemplatesFragment(&bundle.Bundle{Root: root}, def)
	if len(def.Templates) == 0 {
		t.Fatalf("expected templates loaded from templates.yaml, got 0")
	}
	for _, tpl := range def.Templates {
		if tpl.ID == "" || tpl.SystemPrompt == "" || tpl.PreviewPath == "" {
			t.Errorf("incomplete template: %+v", tpl)
		}
	}
	t.Logf("loaded %d templates", len(def.Templates))
}
