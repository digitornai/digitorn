package schema

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModuleBlock_FlatConfigRetrocompat(t *testing.T) {
	var tb ToolsBlock
	if err := yaml.Unmarshal([]byte(`
modules:
  rag:
    backend:
      type: qdrant
      url: localhost:6334
    embedding_model: bge-m3
`), &tb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rag := tb.Modules["rag"]
	if rag.Config["embedding_model"] != "bge-m3" {
		t.Errorf("embedding_model not folded into Config: %+v", rag.Config)
	}
	be, _ := rag.Config["backend"].(map[string]any)
	if be["type"] != "qdrant" || be["url"] != "localhost:6334" {
		t.Errorf("backend not folded: %+v", rag.Config["backend"])
	}
}

func TestModuleBlock_NestedConfigForm(t *testing.T) {
	var tb ToolsBlock
	if err := yaml.Unmarshal([]byte(`
modules:
  rag:
    config:
      backend:
        type: qdrant
      embedding_model: minilm-l12
`), &tb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rag := tb.Modules["rag"]
	if rag.Config["embedding_model"] != "minilm-l12" {
		t.Errorf("nested config lost: %+v", rag.Config)
	}
}

func TestModuleBlock_KnownFieldsNotFolded(t *testing.T) {
	var tb ToolsBlock
	if err := yaml.Unmarshal([]byte(`
modules:
  filesystem:
    config:
      workspace: "."
    constraints:
      allowed_actions: [read]
`), &tb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fs := tb.Modules["filesystem"]
	if fs.Config["workspace"] != "." {
		t.Errorf("config.workspace lost: %+v", fs.Config)
	}
	if _, leaked := fs.Config["constraints"]; leaked {
		t.Error("constraints leaked into Config")
	}
	if fs.Constraints["allowed_actions"] == nil {
		t.Error("constraints not parsed into its own field")
	}
}
