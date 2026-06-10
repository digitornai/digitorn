package rag

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Config is the per-app RAG configuration. Field names + defaults mirror
// the Python module's `tools.modules.rag` YAML so apps written against
// the old documentation resolve identically here. Unknown keys are
// tolerated (forward/backward compatibility) ; only the Phase-1 subset is
// acted on so far, the rest is parsed and carried for later phases.
type Config struct {
	EmbeddingModel EmbeddingModel `json:"embedding_model"`
	Reranker       json.RawMessage `json:"reranker,omitempty"` // bool | string ; Phase 2
	Backend        Backend         `json:"backend"`
	Pipeline       Pipeline        `json:"pipeline"`
	Chunking       Chunking        `json:"chunking"`
	Citations      Citations       `json:"citations"`

	Sources   []SourceConfig `json:"sources"`
	AutoIndex AutoIndex      `json:"auto_index"`
	ACL       ACL            `json:"acl"`

	MaxKnowledgeBases int `json:"max_knowledge_bases"`
	MaxDocuments      int `json:"max_documents"`

	// Computed from Reranker (not read directly from YAML).
	RerankEnabled bool   `json:"-"`
	RerankModel   string `json:"-"`
}

// SourceConfig declares a continuously-synced origin attached to a KB.
// Internal / config-driven — never an agent tool. Keys match the old
// Python sources schema.
type SourceConfig struct {
	Type          string   `json:"type"` // "file" (database/web later)
	Path          string   `json:"path"`
	Extensions    []string `json:"extensions"`
	Recursive     *bool    `json:"recursive"`
	MaxFiles      int      `json:"max_files"`
	KnowledgeBase string   `json:"knowledge_base"`
}

// AutoIndex controls when sources are synced (Python parity).
type AutoIndex struct {
	OnStart  bool   `json:"on_start"`
	Schedule string `json:"schedule"`
}

// EmbeddingModel accepts either a shortcut/id string ("bge-m3") or the
// custom object form ({id, dimensions, pooling, model_file}) the Python
// config allowed. ID resolves through internal/embeddings/models.
type EmbeddingModel struct {
	ID         string `json:"id"`
	Dimensions int    `json:"dimensions,omitempty"`
	Pooling    string `json:"pooling,omitempty"`
}

func (e *EmbeddingModel) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.ID = s
		return nil
	}
	type alias EmbeddingModel
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("embedding_model: want string or object: %w", err)
	}
	*e = EmbeddingModel(a)
	return nil
}

// Backend declares the app's vector server. Keys match the Python
// BackendConfig exactly (type/path/url/api_key/index_name/dsn/…).
type Backend struct {
	Type         string `json:"type"`
	Path         string `json:"path"`
	URL          string `json:"url"`
	APIKey       string `json:"api_key"`
	IndexName    string `json:"index_name"`
	Cloud        string `json:"cloud"`
	Region       string `json:"region"`
	DSN          string `json:"dsn"`
	Quantization string `json:"quantization"`
}

// Pipeline mirrors the Python PipelineConfig.
type Pipeline struct {
	Retrieval      string  `json:"retrieval"`
	BM25Weight     float64 `json:"bm25_weight"`
	SemanticWeight float64 `json:"semantic_weight"`
	RerankTopN     int     `json:"rerank_top_n"`
	FinalTopK      int     `json:"final_top_k"`
}

// Chunking mirrors the Python ChunkingConfig.
type Chunking struct {
	Strategy string `json:"strategy"`
	Size     int    `json:"size"`
	Overlap  int    `json:"overlap"`
}

// Citations mirrors the Python CitationConfig.
type Citations struct {
	Enabled bool   `json:"enabled"`
	Format  string `json:"format"`
	Verify  bool   `json:"verify"`
}

// ParseConfig decodes a per-app config map (tolerating unknown keys) and
// applies the doc defaults. An empty/zero map yields the zero-config
// defaults : qdrant backend, minilm-l12, recursive chunking, hybrid
// pipeline, inline citations.
func ParseConfig(raw map[string]any) (Config, error) {
	var c Config
	if len(raw) > 0 {
		b, err := json.Marshal(raw)
		if err != nil {
			return Config{}, fmt.Errorf("rag config: marshal: %w", err)
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return Config{}, fmt.Errorf("rag config: %w", err)
		}
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.EmbeddingModel.ID) == "" {
		c.EmbeddingModel.ID = "minilm-l12"
	}
	if c.Backend.Type == "" {
		c.Backend.Type = "qdrant"
	}
	if c.Chunking.Strategy == "" {
		c.Chunking.Strategy = StrategyRecursive
	}
	if c.Chunking.Size == 0 {
		c.Chunking.Size = 500
	}
	if c.Chunking.Overlap == 0 {
		c.Chunking.Overlap = 50
	}
	if c.Pipeline.Retrieval == "" {
		c.Pipeline.Retrieval = "hybrid"
	}
	if c.Pipeline.BM25Weight == 0 && c.Pipeline.SemanticWeight == 0 {
		c.Pipeline.BM25Weight, c.Pipeline.SemanticWeight = 0.3, 0.7
	}
	if c.Pipeline.FinalTopK == 0 {
		c.Pipeline.FinalTopK = 5
	}
	if c.Pipeline.RerankTopN == 0 {
		c.Pipeline.RerankTopN = 20
	}
	if c.Citations.Format == "" {
		c.Citations.Format = "inline"
	}
	if c.MaxKnowledgeBases == 0 {
		c.MaxKnowledgeBases = 50
	}
	if c.MaxDocuments == 0 {
		c.MaxDocuments = 100000
	}
	if c.ACL.Enabled {
		if c.ACL.Field == "" {
			c.ACL.Field = "owner"
		}
		if c.ACL.Scope == "" {
			c.ACL.Scope = "user"
		}
	}
	// reranker: true → default model ; reranker: "id" → that model.
	if len(c.Reranker) > 0 {
		var bv bool
		var sv string
		if json.Unmarshal(c.Reranker, &bv) == nil {
			c.RerankEnabled = bv
		} else if json.Unmarshal(c.Reranker, &sv) == nil && strings.TrimSpace(sv) != "" {
			c.RerankEnabled, c.RerankModel = true, strings.TrimSpace(sv)
		}
	}
}
