package dbaccess

import (
	"context"
	"fmt"
	"strings"
)

// Schema introspects the live database (structure + foreign-key graph + column
// comments), optionally samples low-cardinality column values, then overlays
// the operator's business decoration — turning a cryptic schema into context an
// agent can reason over.
func (s *sqlDB) Schema(ctx context.Context) (*Catalog, error) {
	var order []string
	var tables map[string]*TableInfo
	var err error
	switch s.kind {
	case "postgres":
		order, tables, err = s.introspectPostgres(ctx)
	case "mysql":
		order, tables, err = s.introspectMySQL(ctx)
	default:
		return &Catalog{}, nil
	}
	if err != nil {
		return nil, err
	}
	if s.sample {
		s.sampleColumns(ctx, tables)
	}
	cat := &Catalog{}
	for _, name := range order {
		t := tables[name]
		if d, ok := s.decor.Tables[name]; ok {
			applyDecor(t, d)
		}
		cat.Tables = append(cat.Tables, *t)
	}
	return cat, nil
}

func (s *sqlDB) introspectPostgres(ctx context.Context) ([]string, map[string]*TableInfo, error) {
	tables := map[string]*TableInfo{}
	var order []string

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.relname, COALESCE(d.description,'')
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_description d ON d.objoid=c.oid AND d.objsubid=0
		WHERE c.relkind='r' AND n.nspname='public' ORDER BY c.relname`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var name, desc string
		if err := rows.Scan(&name, &desc); err != nil {
			rows.Close()
			return nil, nil, err
		}
		tables[name] = &TableInfo{Name: name, Description: desc}
		order = append(order, name)
	}
	rows.Close()

	crows, err := s.db.QueryContext(ctx, `
		SELECT c.relname, a.attname, format_type(a.atttypid,a.atttypmod), COALESCE(d.description,'')
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		LEFT JOIN pg_description d ON d.objoid=c.oid AND d.objsubid=a.attnum
		WHERE c.relkind='r' AND n.nspname='public' ORDER BY c.relname, a.attnum`)
	if err != nil {
		return nil, nil, err
	}
	for crows.Next() {
		var tbl, col, typ, desc string
		if err := crows.Scan(&tbl, &col, &typ, &desc); err != nil {
			crows.Close()
			return nil, nil, err
		}
		if t := tables[tbl]; t != nil {
			t.Columns = append(t.Columns, ColumnInfo{Name: col, Type: typ, Description: desc})
		}
	}
	crows.Close()

	frows, err := s.db.QueryContext(ctx, `
		SELECT tc.table_name, kcu.column_name, ccu.table_name, ccu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu ON tc.constraint_name=kcu.constraint_name AND tc.table_schema=kcu.table_schema
		JOIN information_schema.constraint_column_usage ccu ON ccu.constraint_name=tc.constraint_name
		WHERE tc.constraint_type='FOREIGN KEY' AND tc.table_schema='public'`)
	if err == nil {
		addFKs(tables, frows)
	}
	return order, tables, nil
}

func (s *sqlDB) introspectMySQL(ctx context.Context) ([]string, map[string]*TableInfo, error) {
	tables := map[string]*TableInfo{}
	var order []string

	rows, err := s.db.QueryContext(ctx, `
		SELECT table_name, COALESCE(table_comment,'')
		FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var name, desc string
		if err := rows.Scan(&name, &desc); err != nil {
			rows.Close()
			return nil, nil, err
		}
		tables[name] = &TableInfo{Name: name, Description: desc}
		order = append(order, name)
	}
	rows.Close()

	crows, err := s.db.QueryContext(ctx, `
		SELECT table_name, column_name, column_type, COALESCE(column_comment,'')
		FROM information_schema.columns WHERE table_schema=DATABASE()
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		return nil, nil, err
	}
	for crows.Next() {
		var tbl, col, typ, desc string
		if err := crows.Scan(&tbl, &col, &typ, &desc); err != nil {
			crows.Close()
			return nil, nil, err
		}
		if t := tables[tbl]; t != nil {
			t.Columns = append(t.Columns, ColumnInfo{Name: col, Type: typ, Description: desc})
		}
	}
	crows.Close()

	frows, err := s.db.QueryContext(ctx, `
		SELECT table_name, column_name, referenced_table_name, referenced_column_name
		FROM information_schema.key_column_usage
		WHERE table_schema=DATABASE() AND referenced_table_name IS NOT NULL`)
	if err == nil {
		addFKs(tables, frows)
	}
	return order, tables, nil
}

type fkScanner interface {
	Next() bool
	Scan(...any) error
	Close() error
}

func addFKs(tables map[string]*TableInfo, rows fkScanner) {
	defer rows.Close()
	for rows.Next() {
		var tbl, col, refTbl, refCol string
		if rows.Scan(&tbl, &col, &refTbl, &refCol) != nil {
			return
		}
		if t := tables[tbl]; t != nil {
			t.Relations = append(t.Relations, fmt.Sprintf("%s.%s → %s.%s", tbl, col, refTbl, refCol))
		}
	}
}

func (s *sqlDB) sampleColumns(ctx context.Context, tables map[string]*TableInfo) {
	for _, t := range tables {
		for i := range t.Columns {
			if !looksEnum(t.Columns[i].Name) {
				continue
			}
			q := fmt.Sprintf("SELECT DISTINCT %s FROM %s LIMIT 12",
				quoteIdent(s.kind, t.Columns[i].Name), quoteIdent(s.kind, t.Name))
			rows, err := s.db.QueryContext(ctx, q)
			if err != nil {
				continue
			}
			var vals []string
			for rows.Next() {
				var v any
				if rows.Scan(&v) == nil {
					if sv := normalizeVal(v); sv != nil {
						vals = append(vals, fmt.Sprint(sv))
					}
				}
			}
			rows.Close()
			t.Columns[i].Samples = vals
		}
	}
}

func looksEnum(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"status", "state", "type", "kind", "sts", "flag", "category", "level", "role", "gender", "currency", "lang", "country", "tier"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func applyDecor(t *TableInfo, d TableDecor) {
	if d.Description != "" {
		t.Description = d.Description
	}
	t.Aka = append(t.Aka, d.Aka...)
	sensitive := map[string]bool{}
	for _, c := range d.Sensitive {
		sensitive[strings.ToLower(c)] = true
	}
	for i := range t.Columns {
		col := &t.Columns[i]
		if desc, ok := d.Columns[col.Name]; ok {
			col.Description = desc
		}
		if sensitive[strings.ToLower(col.Name)] {
			col.Sensitive = true
		}
	}
	t.Relations = append(t.Relations, d.Relations...)
	t.Golden = append(t.Golden, d.Golden...)
}
