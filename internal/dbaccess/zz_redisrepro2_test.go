package dbaccess

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisRepro2(t *testing.T) {
	dsn := redisDSN()
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cli := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cli.Ping(ctx).Err() != nil {
		t.Skipf("no redis at %s", dsn)
	}
	cli.Set(ctx, "greeting", "hello", 0)
	_ = cli.Close()

	mgr := NewManager(0, 0)
	defer mgr.Shutdown()

	cfg := ConnConfig{
		Name: "cache", Kind: "redis", DSN: dsn,
		Security: SecurityPolicy{Mode: "read_only", DeniedStatements: []string{"get", "memory", "keys"}},
	}
	db, err := mgr.Named(ctx, "reproapp", cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	r, errGet := db.Query(ctx, "GET greeting")
	gotVal := ""
	if r != nil && len(r.Rows) == 1 {
		gotVal, _ = r.Rows[0]["value"].(string)
	}
	t.Logf("[CLAIM1] GET greeting denied=[get] -> blocked=%v val=%q err=%v", errGet != nil, gotVal, errGet)

	rp, errPurge := db.Query(ctx, "MEMORY PURGE")
	pv := ""
	if rp != nil && len(rp.Rows) == 1 {
		pv, _ = rp.Rows[0]["value"].(string)
	}
	t.Logf("[CLAIM2] MEMORY PURGE read_only+denied=[memory] -> blocked=%v val=%q err=%v", errPurge != nil, pv, errPurge)

	_, errKeys := db.Query(ctx, "KEYS *")
	t.Logf("[EXTRA] KEYS * denied=[keys] -> blocked=%v err=%v", errKeys != nil, errKeys)

	_, errFlush := db.Query(ctx, "FLUSHDB")
	t.Logf("[CONTROL] FLUSHDB -> blocked=%v err=%v", errFlush != nil, errFlush)

	t.Logf("VERDICT: GET_blocked=%v MEMORY_PURGE_blocked=%v KEYS_blocked=%v FLUSHDB_blocked=%v",
		errGet != nil, errPurge != nil, errKeys != nil, errFlush != nil)
}
