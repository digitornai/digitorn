package rag

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
)

type Config struct {
	EmbeddingModel EmbeddingModel  `json:"embedding_model"`
	Reranker       json.RawMessage `json:"reranker,omitempty"`
	Backend        Backend         `json:"backend"`
	Pipeline       Pipeline        `json:"pipeline"`
	Chunking       Chunking        `json:"chunking"`
	Citations      Citations       `json:"citations"`

	Sources   []SourceConfig `json:"sources"`
	AutoIndex AutoIndex      `json:"auto_index"`
	ACL       ACL            `json:"acl"`
	Cache     CacheConfig    `json:"cache"`

	DefaultKnowledgeBase string `json:"default_knowledge_base"`

	CursorDSN string `json:"cursor_dsn" jsonschema:"title=Cursor DSN,format=password"`

	MaxKnowledgeBases int `json:"max_knowledge_bases"`
	MaxDocuments      int `json:"max_documents"`

	RerankEnabled bool   `json:"-"`
	RerankModel   string `json:"-"`
}

type SourceConfig struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Path          string   `json:"path"`
	Extensions    []string `json:"extensions"`
	Recursive     *bool    `json:"recursive"`
	MaxFiles      int      `json:"max_files"`
	KnowledgeBase string   `json:"knowledge_base"`

	Triggers []TriggerConfig `json:"triggers"`

	DSN            string   `json:"dsn" jsonschema:"title=Source DSN,format=password"`
	Query          string   `json:"query"`
	IDColumn       string   `json:"id_column"`
	TextColumns    []string `json:"text_columns"`
	CDCTable       string   `json:"cdc_table"`
	CDCSlot        string   `json:"cdc_slot"`
	CDCPublication string   `json:"cdc_publication"`

	Brokers    []string `json:"brokers"`
	Topic      string   `json:"topic"`
	GroupID    string   `json:"group_id"`
	IDField    string   `json:"id_field"`
	TextFields []string `json:"text_fields"`

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

	SymbolChunks *bool `json:"symbol_chunks"`
}

type TriggerConfig struct {
	Type  string `json:"type"`
	Every string `json:"every"`
	Cron  string `json:"cron"`
}

type AutoIndex struct {
	OnStart  bool   `json:"on_start"`
	Schedule string `json:"schedule"`
}

type EmbeddingModel struct {
	ID         string `json:"id"`
	Dimensions int    `json:"dimensions,omitempty"`
	Pooling    string `json:"pooling,omitempty"`
}

func (EmbeddingModel) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("id", &jsonschema.Schema{Type: "string"})
	props.Set("dimensions", &jsonschema.Schema{Type: "integer"})
	props.Set("pooling", &jsonschema.Schema{Type: "string"})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Embedding model id or shortcut (e.g. \"minilm-l12\")."},
			{Type: "object", Properties: props},
		},
	}
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

type Pipeline struct {
	Retrieval      string  `json:"retrieval"`
	BM25Weight     float64 `json:"bm25_weight"`
	SemanticWeight float64 `json:"semantic_weight"`
	RerankTopN     int     `json:"rerank_top_n"`
	FinalTopK      int     `json:"final_top_k"`
}

type Chunking struct {
	Strategy string `json:"strategy"`
	Size     int    `json:"size"`
	Overlap  int    `json:"overlap"`
}

type Citations struct {
	Enabled bool   `json:"enabled"`
	Format  string `json:"format"`
	Verify  bool   `json:"verify"`
}

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
