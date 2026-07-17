package dbaccess

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Row = map[string]any

type Result struct {
	Columns  []string `json:"columns,omitempty"`
	Rows     []Row    `json:"rows"`
	RowCount int      `json:"row_count"`
	Truncated bool    `json:"truncated,omitempty"`
}

type DB interface {
	Kind() string
	Query(ctx context.Context, query string, args ...any) (*Result, error)
	Schema(ctx context.Context) (*Catalog, error)
	Close() error
}

type ConnConfig struct {
	Name     string
	Kind     string
	DSN      string
	Security SecurityPolicy
	Decor    SchemaDecor
	SampleValues bool
}

type Streamer interface {
	QueryStream(ctx context.Context, query string, fn func(Row) error) error
}

type Opener func(ctx context.Context, cfg ConnConfig) (DB, error)

var (
	regMu   sync.RWMutex
	openers = map[string]Opener{}
)

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

func Open(ctx context.Context, cfg ConnConfig) (DB, error) {
	o, ok := openerFor(normalizeKind(cfg.Kind))
	if !ok {
		return nil, fmt.Errorf("dbaccess: no engine for kind %q", cfg.Kind)
	}
	return o(ctx, cfg)
}

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

type SecurityPolicy struct {
	Mode             string        `json:"mode"`
	EnforceDBReadOnly bool         `json:"enforce_db_readonly"`
	ApplyRole        string        `json:"apply_role"`
	StatementTimeout time.Duration `json:"statement_timeout"`
	DeniedStatements []string      `json:"denied_statements"`
	AllowedTables    []string      `json:"allowed_tables"`
	MaxRows          int           `json:"max_rows"`
	SensitiveColumns []string      `json:"sensitive_columns"`
	Egress           string        `json:"egress"`
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
