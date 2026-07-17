package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/digitornai/digitorn/internal/dbaccess"
)

type dbaccessConnector struct{ kind string }

func init() {
	for _, k := range []string{"sql", "mysql", "mariadb", "sqlite", "sqlserver", "mssql", "mongodb", "mongo"} {
		Register(&dbaccessConnector{kind: k})
	}
}

func (c *dbaccessConnector) Type() string                                          { return c.kind }
func (c *dbaccessConnector) Capabilities() Caps                                     { return Caps{Walk: true} }
func (c *dbaccessConnector) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }

func (c *dbaccessConnector) Walk(ctx context.Context, spec SourceSpec, emit func(Document) error) error {
	o := parseDBOpts(spec.Opts)
	if o.DSN == "" || o.Query == "" || o.IDColumn == "" {
		return fmt.Errorf("indexer/%s: dsn, query and id_column are required", c.kind)
	}
	db, err := dbaccess.Open(ctx, dbaccess.ConnConfig{
		Kind: c.kind, DSN: o.DSN,
		Security: dbaccess.SecurityPolicy{Mode: "read_only", MaxRows: 1_000_000, StatementTimeout: 10 * time.Minute},
	})
	if err != nil {
		return fmt.Errorf("indexer/%s: open: %w", c.kind, err)
	}
	defer db.Close()

	emitRow := func(row map[string]any) error {
		if doc, ok := o.docFromRow(row); ok {
			return emit(doc)
		}
		return nil
	}

	if st, ok := db.(dbaccess.Streamer); ok {
		return st.QueryStream(ctx, o.Query, emitRow)
	}
	res, err := db.Query(ctx, o.Query)
	if err != nil {
		return fmt.Errorf("indexer/%s: query: %w", c.kind, err)
	}
	for _, row := range res.Rows {
		if err := emitRow(row); err != nil {
			return err
		}
	}
	return nil
}
