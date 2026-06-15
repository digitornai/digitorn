package dbaccess

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestIndep_AllowedTables is my own from-scratch reproduction. It exercises:
//   (a) a table NOT in allowed_tables -> should be rejected if enforced
//   (b) a table IN allowed_tables     -> should succeed if enforced
//   (c) SELECT 1 (no table)           -> baseline
// under Mode=read_only, AllowedTables=["allowed_t"].
func TestIndep_AllowedTables(t *testing.T) {
	const dsn = "postgres://postgres:postgres@127.0.0.1:5433/postgres?sslmode=disable"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rw, err := Open(ctx, ConnConfig{Kind: "postgres", DSN: dsn, Security: SecurityPolicy{Mode: "read_write"}})
	if err != nil {
		t.Skip("pg unreachable:", err)
	}
	// setup two tables: one off-list (off_list_secret), one on-list (allowed_t)
	_, _ = rw.Query(ctx, "DROP TABLE IF EXISTS off_list_secret")
	_, _ = rw.Query(ctx, "DROP TABLE IF EXISTS allowed_t")
	if _, e := rw.Query(ctx, "CREATE TABLE off_list_secret(id int, salary int)"); e != nil {
		rw.Close()
		t.Fatalf("create off_list_secret: %v", e)
	}
	if _, e := rw.Query(ctx, "CREATE TABLE allowed_t(id int)"); e != nil {
		rw.Close()
		t.Fatalf("create allowed_t: %v", e)
	}
	_, _ = rw.Query(ctx, "INSERT INTO off_list_secret VALUES (1, 999999)")
	_, _ = rw.Query(ctx, "INSERT INTO allowed_t VALUES (42)")
	rw.Close()

	db, err := Open(ctx, ConnConfig{Kind: "postgres", DSN: dsn,
		Security: SecurityPolicy{Mode: "read_only", AllowedTables: []string{"allowed_t"}}})
	if err != nil {
		t.Fatalf("open scoped: %v", err)
	}
	defer db.Close()

	type probe struct {
		label string
		sql   string
	}
	for _, p := range []probe{
		{"OFF-LIST table off_list_secret", "SELECT id, salary FROM off_list_secret"},
		{"ON-LIST table allowed_t", "SELECT id FROM allowed_t"},
		{"NO-TABLE SELECT 1", "SELECT 1"},
	} {
		r, qerr := db.Query(ctx, p.sql)
		n := -1
		if r != nil {
			n = len(r.Rows)
		}
		fmt.Printf("[%s] sql=%q -> err=%v rows=%d data=%v\n", p.label, p.sql, qerr, n, func() any {
			if r != nil {
				return r.Rows
			}
			return nil
		}())
	}

	// cleanup
	cl, _ := Open(ctx, ConnConfig{Kind: "postgres", DSN: dsn, Security: SecurityPolicy{Mode: "read_write"}})
	if cl != nil {
		_, _ = cl.Query(ctx, "DROP TABLE IF EXISTS off_list_secret")
		_, _ = cl.Query(ctx, "DROP TABLE IF EXISTS allowed_t")
		cl.Close()
	}
}
