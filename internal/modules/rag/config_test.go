package rag

import (
	"encoding/json"
	"testing"
)

func cfgFromJSON(t *testing.T, s string) Config {
	t.Helper()
	var m map[string]any
	if s != "" {
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("bad test json: %v", err)
		}
	}
	c, err := ParseConfig(m)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return c
}

func TestParseConfig_ZeroConfigDefaults(t *testing.T) {
	c := cfgFromJSON(t, "")
	if c.EmbeddingModel.ID != "minilm-l12" {
		t.Errorf("embedding_model = %q", c.EmbeddingModel.ID)
	}
	if c.Backend.Type != "qdrant" {
		t.Errorf("backend.type = %q", c.Backend.Type)
	}
	if c.Chunking.Strategy != StrategyRecursive || c.Chunking.Size != 500 || c.Chunking.Overlap != 50 {
		t.Errorf("chunking = %+v", c.Chunking)
	}
	if c.Pipeline.Retrieval != "hybrid" || c.Pipeline.FinalTopK != 5 {
		t.Errorf("pipeline = %+v", c.Pipeline)
	}
	if c.Pipeline.SemanticWeight != 0.7 || c.Pipeline.BM25Weight != 0.3 {
		t.Errorf("weights = %+v", c.Pipeline)
	}
	if c.Citations.Format != "inline" || c.MaxKnowledgeBases != 50 {
		t.Errorf("citations/max = %+v %d", c.Citations, c.MaxKnowledgeBases)
	}
}

func TestParseConfig_EmbeddingModelAsString(t *testing.T) {
	c := cfgFromJSON(t, `{"embedding_model":"bge-m3"}`)
	if c.EmbeddingModel.ID != "bge-m3" {
		t.Errorf("got %q", c.EmbeddingModel.ID)
	}
}

func TestParseConfig_EmbeddingModelAsObject(t *testing.T) {
	c := cfgFromJSON(t, `{"embedding_model":{"id":"custom/model","dimensions":768,"pooling":"cls"}}`)
	if c.EmbeddingModel.ID != "custom/model" || c.EmbeddingModel.Dimensions != 768 || c.EmbeddingModel.Pooling != "cls" {
		t.Errorf("got %+v", c.EmbeddingModel)
	}
}

func TestParseConfig_BackendKeysAndUnknownTolerated(t *testing.T) {
	c := cfgFromJSON(t, `{
		"backend":{"type":"qdrant","url":"http://localhost:6333","quantization":"int8"},
		"pipeline":{"retrieval":"semantic","final_top_k":8},
		"some_future_key":{"nested":true}
	}`)
	if c.Backend.Type != "qdrant" || c.Backend.URL != "http://localhost:6333" || c.Backend.Quantization != "int8" {
		t.Errorf("backend = %+v", c.Backend)
	}
	if c.Pipeline.Retrieval != "semantic" || c.Pipeline.FinalTopK != 8 {
		t.Errorf("pipeline = %+v", c.Pipeline)
	}
}

func TestParseConfig_PgvectorDSN(t *testing.T) {
	c := cfgFromJSON(t, `{"backend":{"type":"pgvector","dsn":"postgres://u@/db"}}`)
	if c.Backend.Type != "pgvector" || c.Backend.DSN != "postgres://u@/db" {
		t.Errorf("backend = %+v", c.Backend)
	}
}
