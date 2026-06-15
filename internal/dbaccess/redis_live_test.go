package dbaccess

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisDSN() string {
	if v := os.Getenv("DBACCESS_REDIS_DSN"); v != "" {
		return v
	}
	return "redis://localhost:6380/0"
}

// TestRedisAccess_Live proves Redis on the SAME interface : read commands
// return normalized rows, write/admin commands are blocked under read_only,
// and the keyspace is introspected by prefix.
func TestRedisAccess_Live(t *testing.T) {
	opt, err := redis.ParseURL(redisDSN())
	if err != nil {
		t.Fatal(err)
	}
	cli := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cli.Ping(ctx).Err() != nil {
		t.Skipf("no redis at %s", redisDSN())
	}
	cli.FlushDB(ctx)
	cli.Set(ctx, "greeting", "hello", 0)
	cli.RPush(ctx, "fruits", "apple", "banana", "cherry")
	cli.HSet(ctx, "user:1", "name", "Alice", "ssn", "111-11")
	_ = cli.Close()

	mgr := NewManager(0, 0)
	defer mgr.Shutdown()
	cfg := ConnConfig{
		Name: "cache", Kind: "redis", DSN: redisDSN(),
		Security: SecurityPolicy{Mode: "read_only", SensitiveColumns: []string{"ssn"}},
		Decor:    SchemaDecor{Tables: map[string]TableDecor{"user": {Description: "User cache entries"}}},
	}
	db, err := mgr.Named(ctx, "app1", cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// GET → single value.
	if r, err := db.Query(ctx, "GET greeting"); err != nil || len(r.Rows) != 1 || r.Rows[0]["value"] != "hello" {
		t.Fatalf("GET = %+v err=%v", r, err)
	}
	// LRANGE → list rows.
	if r, err := db.Query(ctx, "LRANGE fruits 0 -1"); err != nil || r.RowCount != 3 || r.Rows[0]["value"] != "apple" {
		t.Fatalf("LRANGE = %+v err=%v", r, err)
	}
	// HGETALL → one row map, with PII masked.
	r, err := db.Query(ctx, "HGETALL user:1")
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("HGETALL = %+v err=%v", r, err)
	}
	if r.Rows[0]["name"] != "Alice" || r.Rows[0]["ssn"] != "***" {
		t.Fatalf("HGETALL masking failed: %+v", r.Rows[0])
	}

	// read_only blocks writes + admin.
	for _, bad := range []string{"SET greeting bye", "DEL greeting", "FLUSHDB", "CONFIG GET maxmemory"} {
		if _, err := db.Query(ctx, bad); err == nil {
			t.Fatalf("read_only let %q through", bad)
		}
	}

	// Keyspace introspection by prefix.
	cat, err := db.Schema(ctx)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var userNS *TableInfo
	for i := range cat.Tables {
		if cat.Tables[i].Name == "user" {
			userNS = &cat.Tables[i]
		}
	}
	if userNS == nil || userNS.Description != "User cache entries" {
		t.Fatalf("keyspace prefix not introspected/decorated: %+v", cat.Tables)
	}
	t.Logf("redis: prefixes=%d", len(cat.Tables))
}
