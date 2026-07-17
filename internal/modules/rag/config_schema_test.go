package rag

import (
	"encoding/json"
	"testing"

	"github.com/digitornai/digitorn/pkg/module"
)

func TestEmbeddingModelSchemaAcceptsStringAndObject(t *testing.T) {
	m := module.SchemaFromType(&Config{})
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", m)
	}
	raw, ok := props["embedding_model"]
	if !ok {
		t.Fatalf("schema has no embedding_model property")
	}
	b, _ := json.Marshal(raw)
	var em map[string]any
	if err := json.Unmarshal(b, &em); err != nil {
		t.Fatalf("embedding_model schema not an object: %v", err)
	}
	oneOf, ok := em["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("embedding_model schema must be a oneOf [string, object], got: %s", b)
	}
	types := map[string]bool{}
	for _, v := range oneOf {
		if sub, ok := v.(map[string]any); ok {
			if ty, _ := sub["type"].(string); ty != "" {
				types[ty] = true
			}
		}
	}
	if !types["string"] || !types["object"] {
		t.Fatalf("oneOf must contain string and object variants, got: %s", b)
	}
	if ty, _ := em["type"].(string); ty != "" {
		t.Fatalf("embedding_model schema must not pin a top-level type (validator would enforce it), got type=%q", ty)
	}
}

func TestEmbeddingModelUnmarshalBothForms(t *testing.T) {
	var s EmbeddingModel
	if err := json.Unmarshal([]byte(`"minilm-l12"`), &s); err != nil || s.ID != "minilm-l12" {
		t.Fatalf("string form: err=%v id=%q", err, s.ID)
	}
	var o EmbeddingModel
	if err := json.Unmarshal([]byte(`{"id":"bge-m3","dimensions":1024}`), &o); err != nil || o.ID != "bge-m3" || o.Dimensions != 1024 {
		t.Fatalf("object form: err=%v %+v", err, o)
	}
}
