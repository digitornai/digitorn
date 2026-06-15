package dbaccess

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func seed(t *testing.T, kind, dsn string, stmts []string) bool {
	driver, d, err := sqlDriverDSN(kind, dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driver, d)
	if err != nil {
		return false
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		return false
	}
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return true
}

// TestSQLAccess_Live proves the universal SQL access layer end-to-end against
// REAL remote Postgres + MySQL : pooled connect, guarded read-only query with
// PII masking, defense-in-depth write blocking (statement guard AND DB-side
// read-only transaction), row caps, and decorated schema introspection
// (FK graph + comments + value samples + business overlay).
func TestSQLAccess_Live(t *testing.T) {
	cases := []struct {
		name, kind, dsn string
		seedStmts       []string
	}{
		{
			"postgres", "postgres", envOr("DBACCESS_PG_DSN", "postgres://postgres:postgres@localhost:5433/postgres"),
			[]string{
				"DROP TABLE IF EXISTS orders", "DROP TABLE IF EXISTS customers",
				"CREATE TABLE customers (id int PRIMARY KEY, name text, ssn text, status text)",
				"COMMENT ON TABLE customers IS 'raw db comment'",
				"COMMENT ON COLUMN customers.status IS 'db status comment'",
				"CREATE TABLE orders (id int PRIMARY KEY, customer_id int REFERENCES customers(id), amount numeric)",
				"INSERT INTO customers VALUES (1,'Alice','111-11-1111','A'),(2,'Bob','222-22-2222','C'),(3,'Carol','333-33-3333','A')",
				"INSERT INTO orders VALUES (10,1,99.5),(11,2,12.0)",
			},
		},
		{
			"mysql", "mysql", envOr("DBACCESS_MYSQL_DSN", "mysql://root:root@localhost:3307/ragtest"),
			[]string{
				"DROP TABLE IF EXISTS orders", "DROP TABLE IF EXISTS customers",
				"CREATE TABLE customers (id int PRIMARY KEY, name varchar(64), ssn varchar(32), status varchar(8) COMMENT 'db status comment') COMMENT='raw db comment'",
				"CREATE TABLE orders (id int PRIMARY KEY, customer_id int, amount decimal(10,2), FOREIGN KEY (customer_id) REFERENCES customers(id))",
				"INSERT INTO customers VALUES (1,'Alice','111-11-1111','A'),(2,'Bob','222-22-2222','C'),(3,'Carol','333-33-3333','A')",
				"INSERT INTO orders VALUES (10,1,99.5),(11,2,12.0)",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !seed(t, tc.kind, tc.dsn, tc.seedStmts) {
				t.Skipf("no %s at %s", tc.kind, tc.dsn)
			}
			ctx := context.Background()
			mgr := NewManager(0, 0)
			defer mgr.Shutdown()

			cfg := ConnConfig{
				Name: "prod", Kind: tc.kind, DSN: tc.dsn, SampleValues: true,
				Security: SecurityPolicy{Mode: "read_only", MaxRows: 100, SensitiveColumns: []string{"ssn"}, StatementTimeout: 5 * time.Second},
				Decor: SchemaDecor{Tables: map[string]TableDecor{
					"customers": {
						Description: "Master customer accounts — one row per client",
						Aka:         []string{"clients"},
						Columns:     map[string]string{"status": "A=active, C=closed"},
						Sensitive:   []string{"ssn"},
						Golden:      []GoldenQuery{{Q: "active customers", SQL: "SELECT * FROM customers WHERE status='A'"}},
					},
				}},
			}

			// Pooled named connection — reused, no per-call reconnect.
			db, err := mgr.Named(ctx, "app1", cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			if db2, _ := mgr.Named(ctx, "app1", cfg); db2 != db {
				t.Fatal("named connection was not pooled (reopened)")
			}

			// SELECT works + PII masking.
			res, err := db.Query(ctx, "SELECT id, name, ssn, status FROM customers ORDER BY id")
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if res.RowCount != 3 {
				t.Fatalf("rows = %d, want 3", res.RowCount)
			}
			if res.Rows[0]["ssn"] != "***" {
				t.Fatalf("ssn not masked: %v", res.Rows[0]["ssn"])
			}
			if res.Rows[0]["name"] != "Alice" {
				t.Fatalf("name = %v, want Alice", res.Rows[0]["name"])
			}

			// Write blocked by the STATEMENT GUARD (read_only).
			for _, bad := range []string{
				"DELETE FROM customers",
				"UPDATE customers SET name='x'",
				"DROP TABLE customers",
				"SELECT 1; DROP TABLE customers",
				"SELECT * FROM customers WHERE name='ok'; DELETE FROM customers",
			} {
				if _, err := db.Query(ctx, bad); err == nil {
					t.Fatalf("guard let a write through: %q", bad)
				}
			}

			// Defense in depth: a write that PASSES the guard (read_write mode)
			// is still rejected by the DB-side READ ONLY transaction.
			roCfg := cfg
			roCfg.Name = "prod_dbro"
			roCfg.Security = SecurityPolicy{Mode: "read_write", EnforceDBReadOnly: true, StatementTimeout: 5 * time.Second}
			dbro, err := mgr.Named(ctx, "app1", roCfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dbro.Query(ctx, "DELETE FROM customers WHERE id=999"); err == nil {
				t.Fatal("DB-side read-only backstop failed: a write was accepted")
			}

			// Row cap / truncation.
			capCfg := cfg
			capCfg.Name = "prod_cap"
			capCfg.Security = SecurityPolicy{Mode: "read_only", MaxRows: 2}
			dbcap, _ := mgr.Named(ctx, "app1", capCfg)
			if r, _ := dbcap.Query(ctx, "SELECT id FROM customers"); r == nil || !r.Truncated || r.RowCount != 2 {
				t.Fatalf("max_rows cap failed: %+v", r)
			}

			// Decorated schema introspection.
			cat, err := db.Schema(ctx)
			if err != nil {
				t.Fatalf("schema: %v", err)
			}
			cust := findTable(cat, "customers")
			ord := findTable(cat, "orders")
			if cust == nil || ord == nil {
				t.Fatalf("missing tables in catalog: %+v", cat.Tables)
			}
			if cust.Description != "Master customer accounts — one row per client" {
				t.Fatalf("decor description not applied: %q", cust.Description)
			}
			if len(cust.Aka) == 0 || cust.Aka[0] != "clients" {
				t.Fatalf("aka not applied: %v", cust.Aka)
			}
			statusCol := findCol(cust, "status")
			if statusCol == nil || statusCol.Description != "A=active, C=closed" {
				t.Fatalf("column decor not applied: %+v", statusCol)
			}
			if len(statusCol.Samples) == 0 {
				t.Fatalf("value samples not collected for status")
			}
			if ssn := findCol(cust, "ssn"); ssn == nil || !ssn.Sensitive {
				t.Fatalf("ssn not flagged sensitive: %+v", ssn)
			}
			if !hasRelation(ord, "customers") {
				t.Fatalf("FK graph missing orders→customers: %v", ord.Relations)
			}
			if len(cust.Golden) == 0 {
				t.Fatalf("golden queries not carried")
			}
			t.Logf("%s: catalog ok — customers(%d cols, samples=%v) orders.relations=%v",
				tc.kind, len(cust.Columns), statusCol.Samples, ord.Relations)
		})
	}
}

func findTable(c *Catalog, name string) *TableInfo {
	for i := range c.Tables {
		if c.Tables[i].Name == name {
			return &c.Tables[i]
		}
	}
	return nil
}

func findCol(t *TableInfo, name string) *ColumnInfo {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

func hasRelation(t *TableInfo, ref string) bool {
	for _, r := range t.Relations {
		if strings.Contains(r, ref) {
			return true
		}
	}
	return false
}
