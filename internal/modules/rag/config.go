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
	EmbeddingModel EmbeddingModel  `json:"embedding_model"`
	Reranker       json.RawMessage `json:"reranker,omitempty"` // bool | string ; Phase 2
	Backend        Backend         `json:"backend"`
	Pipeline       Pipeline        `json:"pipeline"`
	Chunking       Chunking        `json:"chunking"`
	Citations      Citations       `json:"citations"`

	Sources   []SourceConfig `json:"sources"`
	AutoIndex AutoIndex      `json:"auto_index"`
	ACL       ACL            `json:"acl"`
	Cache     CacheConfig    `json:"cache"`

	// DefaultKnowledgeBase is the KB a query routes to when the caller does
	// not name one. Empty → the engine searches every KB the app's sources
	// declare (auto-routing : the agent need not know the index name).
	DefaultKnowledgeBase string `json:"default_knowledge_base"`

	// CursorDSN places this app's indexer sync-state (Walk hashes, CDC LSN,
	// distributed lease) in the app's OWN database — set it, or leave empty to
	// reuse the pgvector backend DSN, so a client can keep everything (index +
	// state) on their infra with nothing local. See indexer.PgStore.
	CursorDSN string `json:"cursor_dsn" jsonschema:"title=Cursor DSN,format=password"`

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
	Name          string   `json:"name"`
	Type          string   `json:"type"` // "file" | "database" | "web"
	Path          string   `json:"path"`
	Extensions    []string `json:"extensions"`
	Recursive     *bool    `json:"recursive"`
	MaxFiles      int      `json:"max_files"`
	KnowledgeBase string   `json:"knowledge_base"`

	// Per-source triggers (when to (re)sync). Empty → falls back to the
	// app-global auto_index (backward compat). See indexer/DESIGN.md.
	Triggers []TriggerConfig `json:"triggers"`

	// Database source (type: "database"). Walk = Query ; CDC (trigger
	// type: cdc) streams CDCTable's WAL in real time.
	DSN            string   `json:"dsn" jsonschema:"title=Source DSN,format=password"`
	Query          string   `json:"query"`
	IDColumn       string   `json:"id_column"`
	TextColumns    []string `json:"text_columns"`
	CDCTable       string   `json:"cdc_table"`
	CDCSlot        string   `json:"cdc_slot"`
	CDCPublication string   `json:"cdc_publication"`

	// Kafka source (type: "kafka", continuous stream).
	Brokers    []string `json:"brokers"`
	Topic      string   `json:"topic"`
	GroupID    string   `json:"group_id"`
	IDField    string   `json:"id_field"`
	TextFields []string `json:"text_fields"`

	// Web source (type: "web"). URL is the seed; the rest mirror the crawler.
	URL           string   `json:"url"`
	MaxPages      int      `json:"max_pages"`
	MaxDepth      int      `json:"max_depth"`
	SameDomain    *bool    `json:"same_domain"`
	Sitemap       *bool    `json:"sitemap"`
	RespectRobots *bool    `json:"respect_robots"`
	AllowPrivate  *bool    `json:"allow_private"`
	RateLimit     string   `json:"rate_limit"`
	Parallelism   int      `json:"parallelism"`
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`

	// Codebase source (type: "codebase"). Path = repo root.
	SymbolChunks *bool `json:"symbol_chunks"`
}

// TriggerConfig is one YAML trigger : {type, every, cron}. type ∈
// on_start | interval | cron | cdc | watch | manual.
type TriggerConfig struct {
	Type  string `json:"type"`
	Every string `json:"every"` // Go duration for interval
	Cron  string `json:"cron"`
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
	Type         string `json:"type" jsonschema:"title=Type,enum=qdrant,enum=pgvector,enum=elasticsearch,enum=memory"`
	Path         string `json:"path" jsonschema:"title=Path"`
	URL          string `json:"url" jsonschema:"title=URL"`
	APIKey       string `json:"api_key" jsonschema:"title=API key,format=password"`
	IndexName    string `json:"index_name" jsonschema:"title=Index name"`
	Cloud        string `json:"cloud" jsonschema:"title=Cloud"`
	Region       string `json:"region" jsonschema:"title=Region"`
	DSN          string `json:"dsn" jsonschema:"title=Connection string (DSN),format=password"`
	Quantization string `json:"quantization" jsonschema:"title=Quantization"`
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
	if c.Cache.Enabled {
		if c.Cache.Threshold == 0 {
			c.Cache.Threshold = 0.97
		}
		if c.Cache.MaxEntries == 0 {
			c.Cache.MaxEntries = 512
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
