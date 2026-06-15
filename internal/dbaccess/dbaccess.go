// Package dbaccess is Digitorn's universal database access layer. ONE Go
// interface (Query/Schema/Close) fronts every engine — all SQL dialects
// through database/sql, plus NoSQL (MongoDB, Redis, Cassandra, Elasticsearch)
// — so a single socle serves BOTH the indexer (Walk a source into the RAG) and
// the agent-facing `database` module (connect/disconnect/query). Adding an
// engine is one opener, not a new connector. Connections are pooled + bounded
// here (off the daemon), and every query passes a configurable, layered
// security policy.
package dbaccess

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Row is one result record — a column/field map that fits SQL rows AND NoSQL
// documents uniformly.
type Row = map[string]any

// Result is the uniform shape returned by every engine's Query.
type Result struct {
	Columns  []string `json:"columns,omitempty"`
	Rows     []Row    `json:"rows"`
	RowCount int      `json:"row_count"`
	Truncated bool    `json:"truncated,omitempty"` // capped at MaxRows
}

// DB is one open, pooled connection to a database of any kind.
type DB interface {
	Kind() string
	Query(ctx context.Context, query string, args ...any) (*Result, error)
	Schema(ctx context.Context) (*Catalog, error)
	Close() error
}

// ConnConfig fully describes a connection : which engine, where, how it is
// secured, and the business decoration overlaid on its schema.
type ConnConfig struct {
	Name     string
	Kind     string // postgres | mysql | sqlite | sqlserver | clickhouse | mongodb | redis | cassandra | elasticsearch
	DSN      string
	Security SecurityPolicy
	Decor    SchemaDecor
	SampleValues bool // introspection: sample low-cardinality column values
}

// Streamer is an optional DB capability : iterate a result row-by-row without
// materializing it. The indexer uses this to Walk a huge table/collection into
// the RAG with bounded memory ; the agent `query` tool uses the capped Query.
type Streamer interface {
	QueryStream(ctx context.Context, query string, fn func(Row) error) error
}

// Opener builds a DB for one kind. Registering an opener makes that engine
// available to the agent AND the indexer at once.
type Opener func(ctx context.Context, cfg ConnConfig) (DB, error)

var (
	regMu   sync.RWMutex
	openers = map[string]Opener{}
)

// Register wires an engine kind to its opener (called from each engine's
// init()). Aliases (postgres/postgresql/pg) register the same opener.
func Register(kind string, o Opener) {
	regMu.Lock()
	openers[kind] = o
	regMu.Unlock()
}

func openerFor(kind string) (Opener, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	o, ok := openers[kind]
	return o, ok
}

// Open resolves the engine and opens a connection, applying the connection's
// session-level security policy. It does NOT pool — the Manager does.
func Open(ctx context.Context, cfg ConnConfig) (DB, error) {
	o, ok := openerFor(normalizeKind(cfg.Kind))
	if !ok {
		return nil, fmt.Errorf("dbaccess: no engine for kind %q", cfg.Kind)
	}
	return o(ctx, cfg)
}

// Kinds returns the registered engine kinds (observability / tooling).
func Kinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(openers))
	for k := range openers {
		out = append(out, k)
	}
	return out
}

func normalizeKind(k string) string {
	switch k {
	case "postgresql", "pg", "postgres":
		return "postgres"
	case "mariadb", "mysql":
		return "mysql"
	case "mongo", "mongodb":
		return "mongodb"
	case "es", "elastic", "elasticsearch":
		return "elasticsearch"
	}
	return k
}

// SecurityPolicy is the configurable, layered guard applied to a connection :
// a DB-side read-only transaction + a statement guard + caps + masking + an
// egress guard. Each layer backstops the others.
type SecurityPolicy struct {
	Mode             string        `json:"mode"`               // read_only | read_write | read_write_approval
	EnforceDBReadOnly bool         `json:"enforce_db_readonly"` // run queries in a DB READ ONLY transaction
	ApplyRole        string        `json:"apply_role"`         // SET ROLE on connect (least privilege)
	StatementTimeout time.Duration `json:"statement_timeout"`
	DeniedStatements []string      `json:"denied_statements"`  // drop/truncate/alter/grant/…
	AllowedTables    []string      `json:"allowed_tables"`
	MaxRows          int           `json:"max_rows"`
	SensitiveColumns []string      `json:"sensitive_columns"`  // masked in results
	Egress           string        `json:"egress"`             // guarded | open
}

func (p SecurityPolicy) readOnly() bool {
	return p.Mode == "" || p.Mode == "read_only"
}

func (p SecurityPolicy) maxRows() int {
	if p.MaxRows > 0 {
		return p.MaxRows
	}
	return 1000
}

func (p SecurityPolicy) timeout() time.Duration {
	if p.StatementTimeout > 0 {
		return p.StatementTimeout
	}
	return 30 * time.Second
}

// Catalog is the decorated schema handed to the agent : tables, columns,
// relationships and business meaning, so a cryptically-named database becomes
// legible.
type Catalog struct {
	Tables []TableInfo `json:"tables"`
}

type TableInfo struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Aka         []string     `json:"aka,omitempty"`
	Columns     []ColumnInfo `json:"columns"`
	Relations   []string     `json:"relations,omitempty"`
	Golden      []GoldenQuery `json:"golden_queries,omitempty"`
}

type ColumnInfo struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Samples     []string `json:"samples,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
}

// SchemaDecor is the operator's business overlay, merged onto the introspected
// structure.
type SchemaDecor struct {
	Tables map[string]TableDecor `json:"tables"`
}

type TableDecor struct {
	Description string            `json:"description"`
	Aka         []string          `json:"aka"`
	Columns     map[string]string `json:"columns"`
	Relations   []string          `json:"relations"`
	Golden      []GoldenQuery     `json:"golden_queries"`
	Sensitive   []string          `json:"sensitive"`
}

type GoldenQuery struct {
	Q   string `json:"q"`
	SQL string `json:"sql"`
}
