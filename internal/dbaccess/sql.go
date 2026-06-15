package dbaccess

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func init() {
	for _, k := range []string{"postgres", "mysql"} {
		Register(k, openSQL)
	}
	// SQLite / SQL Server / ClickHouse are one blank-import + a case in
	// sqlDriverDSN away — the framework already supports them.
}

type sqlDB struct {
	db     *sql.DB
	kind   string
	pol    SecurityPolicy
	decor  SchemaDecor
	sample bool
}

func (s *sqlDB) Kind() string  { return s.kind }
func (s *sqlDB) Close() error  { return s.db.Close() }

func sqlDriverDSN(kind, dsn string) (driver, out string, err error) {
	switch normalizeKind(kind) {
	case "postgres":
		return "pgx", dsn, nil
	case "mysql":
		return "mysql", mysqlNativeDSN(dsn), nil
	}
	return "", "", fmt.Errorf("dbaccess/sql: unsupported sql kind %q", kind)
}

func mysqlNativeDSN(dsn string) string {
	if !strings.HasPrefix(dsn, "mysql://") {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	auth := u.User.Username()
	if p, ok := u.User.Password(); ok {
		auth += ":" + p
	}
	out := auth + "@tcp(" + u.Host + ")" + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func openSQL(ctx context.Context, cfg ConnConfig) (DB, error) {
	if err := guardEgress(cfg.Kind, cfg.DSN, cfg.Security); err != nil {
		return nil, err
	}
	driver, dsn, err := sqlDriverDSN(cfg.Kind, cfg.DSN)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("dbaccess/sql: open %s: %w", cfg.Kind, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("dbaccess/sql: ping %s: %w", cfg.Kind, err)
	}

	pol := cfg.Security
	for _, td := range cfg.Decor.Tables {
		pol.SensitiveColumns = append(pol.SensitiveColumns, td.Sensitive...)
	}
	s := &sqlDB{db: db, kind: normalizeKind(cfg.Kind), pol: pol, decor: cfg.Decor, sample: cfg.SampleValues}
	if err := s.applySession(pctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// applySession pushes the policy into the DB session ONCE at connect : a
// least-privilege role (fail-closed) and a server-side statement timeout +
// read-only default (best-effort ; the per-query READ ONLY transaction is the
// guarantee).
func (s *sqlDB) applySession(ctx context.Context) error {
	if r := strings.TrimSpace(s.pol.ApplyRole); r != "" {
		if _, err := s.db.ExecContext(ctx, "SET ROLE "+quoteIdent(s.kind, r)); err != nil {
			return fmt.Errorf("dbaccess/sql: apply_role %q failed: %w", r, err)
		}
	}
	ms := int(s.pol.timeout() / time.Millisecond)
	switch s.kind {
	case "postgres":
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d", ms))
		if s.pol.readOnly() {
			_, _ = s.db.ExecContext(ctx, "SET default_transaction_read_only = on")
		}
	case "mysql":
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf("SET SESSION max_execution_time = %d", ms))
	}
	return nil
}

func quoteIdent(kind, id string) string {
	id = strings.ReplaceAll(id, `"`, "")
	id = strings.ReplaceAll(id, "`", "")
	if kind == "mysql" {
		return "`" + id + "`"
	}
	return `"` + id + `"`
}

func (s *sqlDB) Query(ctx context.Context, q string, args ...any) (*Result, error) {
	if err := guardStatement(q, s.pol); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.pol.timeout())
	defer cancel()

	var rows *sql.Rows
	var err error
	if s.pol.readOnly() || s.pol.EnforceDBReadOnly {
		tx, terr := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if terr != nil {
			return nil, terr
		}
		defer tx.Rollback()
		rows, err = tx.QueryContext(ctx, q, args...)
	} else {
		rows, err = s.db.QueryContext(ctx, q, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows, s.pol)
}

func scanRows(rows *sql.Rows, pol SecurityPolicy) (*Result, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	sensitive := sensitiveSet(pol)
	max := pol.maxRows()
	res := &Result{Columns: cols, Rows: []Row{}}
	for rows.Next() {
		if len(res.Rows) >= max {
			res.Truncated = true
			break
		}
		row, err := scanRowMap(rows, cols, sensitive)
		if err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, row)
	}
	res.RowCount = len(res.Rows)
	return res, rows.Err()
}

func sensitiveSet(pol SecurityPolicy) map[string]bool {
	m := map[string]bool{}
	for _, c := range pol.SensitiveColumns {
		m[strings.ToLower(c)] = true
	}
	return m
}

func scanRowMap(rows *sql.Rows, cols []string, sensitive map[string]bool) (Row, error) {
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	row := make(Row, len(cols))
	for i, c := range cols {
		if sensitive[strings.ToLower(c)] {
			row[c] = "***"
			continue
		}
		row[c] = normalizeVal(vals[i])
	}
	return row, nil
}

// QueryStream iterates a read query row-by-row (no row cap), for indexing a
// large table with bounded memory. The statement guard + read-only transaction
// still apply ; the caller's ctx bounds the duration.
func (s *sqlDB) QueryStream(ctx context.Context, q string, fn func(Row) error) error {
	if err := guardStatement(q, s.pol); err != nil {
		return err
	}
	var rows *sql.Rows
	var err error
	if s.pol.readOnly() || s.pol.EnforceDBReadOnly {
		tx, terr := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if terr != nil {
			return terr
		}
		defer tx.Rollback()
		rows, err = tx.QueryContext(ctx, q)
	} else {
		rows, err = s.db.QueryContext(ctx, q)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	sensitive := sensitiveSet(s.pol)
	for rows.Next() {
		row, err := scanRowMap(rows, cols, sensitive)
		if err != nil {
			return err
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

func normalizeVal(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
