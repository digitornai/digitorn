package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/dbaccess"
	"github.com/digitornai/digitorn/pkg/module"
)

func pgDSN() string {
	if v := os.Getenv("DBACCESS_PG_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5433/postgres"
}

func TestDatabaseModule_Live(t *testing.T) {
	raw, err := sql.Open("pgx", pgDSN())
	if err != nil {
		t.Fatal(err)
	}
	ctx0, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if raw.PingContext(ctx0) != nil {
		t.Skipf("no Postgres at %s", pgDSN())
	}
	for _, s := range []string{
		"DROP TABLE IF EXISTS accounts",
		"CREATE TABLE accounts (id int PRIMARY KEY, name text, ssn text, status text)",
		"INSERT INTO accounts VALUES (1,'Alice','111-11','A'),(2,'Bob','222-22','C')",
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	raw.Close()

	cfg := Config{Databases: []DBConn{{
		Name: "prod", Kind: "postgres", DSN: pgDSN(), SampleValues: true,
		Security: SecurityConfig{Mode: "read_only", MaxRows: 100, SensitiveColumns: []string{"ssn"}},
		Schema:   SchemaConfig{Tables: map[string]TableConfig{"accounts": {Description: "Customer accounts", Aka: []string{"clients"}}}},
	}}}
	b, _ := json.Marshal(cfg)
	var cfgMap map[string]any
	_ = json.Unmarshal(b, &cfgMap)
	ctx := module.WithModuleConfig(module.WithAppID(context.Background(), "app1"), cfgMap)

	m := New()
	defer m.mgr.Shutdown()

	r, _ := m.connect(ctx, json.RawMessage(`{"name":"prod"}`))
	if !r.Success {
		t.Fatalf("connect failed: %s", r.Error)
	}
	cat, ok := r.Data.(map[string]any)["schema"].(*dbaccess.Catalog)
	if !ok || len(cat.Tables) == 0 || cat.Tables[0].Description != "Customer accounts" {
		t.Fatalf("connect did not return a decorated schema: %+v", r.Data)
	}

	r, _ = m.query(ctx, json.RawMessage(`{"query":"SELECT id, name, ssn FROM accounts ORDER BY id"}`))
	if !r.Success {
		t.Fatalf("query failed: %s", r.Error)
	}
	rows := r.Data.(map[string]any)["rows"].([]dbaccess.Row)
	if len(rows) != 2 || rows[0]["ssn"] != "***" || rows[0]["name"] != "Alice" {
		t.Fatalf("query rows wrong (masking?): %+v", rows)
	}

	// write blocked by the read-only policy.
	if r, _ := m.query(ctx, json.RawMessage(`{"query":"DELETE FROM accounts"}`)); r.Success {
		t.Fatal("read_only policy let a DELETE through the query tool")
	}

	// disconnect.
	if r, _ := m.disconnect(ctx, json.RawMessage(`{"connection":"prod"}`)); !r.Success {
		t.Fatalf("disconnect failed: %s", r.Error)
	}
}
