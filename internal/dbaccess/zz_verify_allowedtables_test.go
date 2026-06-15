package dbaccess

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestVerify_AllowedTablesNotEnforced reproduces the claimed bug from scratch:
// a policy scoped to allowed_tables=["onlythis"] must (if enforced) reject a
// query against a different real table. We create a real "secret_payroll"
// table, then query it under a policy that only allows "onlythis".
func TestVerify_AllowedTablesNotEnforced(t *testing.T) {
	const fmPgDSN = "postgres://postgres:postgres@127.0.0.1:5433/postgres?sslmode=disable"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rw, err := Open(ctx, ConnConfig{Kind: "postgres", DSN: fmPgDSN, Security: SecurityPolicy{Mode: "read_write"}})
	if err != nil {
		t.Skip("pg unreachable:", err)
	}
	_, _ = rw.Query(ctx, "DROP TABLE secret_payroll")
	if _, e := rw.Query(ctx, "CREATE TABLE secret_payroll(id int, salary int)"); e != nil {
		rw.Close()
		t.Fatalf("setup create failed: %v", e)
	}
	_, _ = rw.Query(ctx, "INSERT INTO secret_payroll VALUES (1, 999999)")
	rw.Close()

	// Policy says: only "onlythis" is allowed. We query secret_payroll, NOT in the list.
	db, err := Open(ctx, ConnConfig{Kind: "postgres", DSN: fmPgDSN,
		Security: SecurityPolicy{Mode: "read_only", AllowedTables: []string{"onlythis"}}})
	if err != nil {
		t.Fatalf("open scoped conn: %v", err)
	}
	defer db.Close()

	r, qerr := db.Query(ctx, "SELECT id, salary FROM secret_payroll")
	fmt.Printf("RESULT querying secret_payroll under allowed_tables=[onlythis]: err=%v rows=%v\n",
		qerr, func() any {
			if r != nil {
				return r.Rows
			}
			return nil
		}())

	if qerr == nil && r != nil && len(r.Rows) > 0 {
		fmt.Printf("VERDICT: BUG CONFIRMED - allowed_tables is NOT enforced; off-list table returned %d row(s) including salary data\n", len(r.Rows))
	} else {
		fmt.Printf("VERDICT: query was blocked or empty (err=%v) - allowed_tables MAY be enforced\n", qerr)
	}

	// cleanup
	cl, _ := Open(ctx, ConnConfig{Kind: "postgres", DSN: fmPgDSN, Security: SecurityPolicy{Mode: "read_write"}})
	if cl != nil {
		_, _ = cl.Query(ctx, "DROP TABLE secret_payroll")
		cl.Close()
	}
}
