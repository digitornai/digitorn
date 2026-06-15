package dbaccess

import "testing"

func TestGuardStatement_ReadOnly(t *testing.T) {
	ro := SecurityPolicy{Mode: "read_only"}
	allow := []string{
		"SELECT * FROM t",
		"select id from t where name='delete me'", // write word only inside a string
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT 1 -- ; DROP TABLE t",              // tail is a comment
		"SELECT 1 /* DROP TABLE t */ FROM t",      // write word only inside a comment
		"SHOW TABLES",
		"EXPLAIN SELECT * FROM t",
	}
	for _, q := range allow {
		if err := guardStatement(q, ro); err != nil {
			t.Errorf("should ALLOW %q: %v", q, err)
		}
	}
	deny := []string{
		"DELETE FROM t",
		"UPDATE t SET a=1",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE t",
		"TRUNCATE t",
		"SELECT 1; DROP TABLE t",                              // multi-statement
		"SELECT * FROM t; DELETE FROM t",                      // multi-statement
		"WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x", // data-modifying CTE
		"GRANT ALL ON t TO bob",
	}
	for _, q := range deny {
		if err := guardStatement(q, ro); err == nil {
			t.Errorf("should DENY %q", q)
		}
	}
}

func TestGuardStatement_ReadWriteWithDenied(t *testing.T) {
	pol := SecurityPolicy{Mode: "read_write", DeniedStatements: []string{"drop", "truncate", "delete"}}
	if err := guardStatement("INSERT INTO t VALUES (1)", pol); err != nil {
		t.Errorf("read_write should allow INSERT: %v", err)
	}
	if err := guardStatement("UPDATE t SET a=1", pol); err != nil {
		t.Errorf("read_write should allow UPDATE: %v", err)
	}
	for _, q := range []string{"DROP TABLE t", "TRUNCATE t", "DELETE FROM t"} {
		if err := guardStatement(q, pol); err == nil {
			t.Errorf("denied_statements should block %q", q)
		}
	}
}

func TestHostFromDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@db.example.com:5432/x":   "db.example.com",
		"mongodb://u:p@10.0.0.5:27017/x":         "10.0.0.5",
		"root:root@tcp(mysql.internal:3306)/rag": "mysql.internal",
	}
	for dsn, want := range cases {
		kind := "postgres"
		if want == "mysql.internal" {
			kind = "mysql"
		}
		if got := hostFromDSN(kind, dsn); got != want {
			t.Errorf("hostFromDSN(%q) = %q, want %q", dsn, got, want)
		}
	}
}
