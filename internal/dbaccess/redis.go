package dbaccess

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func init() { Register("redis", openRedis) }

// redisDB fronts Redis on the uniform DB interface : the query string is a
// Redis command ("GET k", "HGETALL h", "LRANGE l 0 -1", "SCAN 0 MATCH p:*"),
// run via Do and normalized to Rows. read_only allows only read commands ;
// destructive/admin commands are always denied. "Schema" describes the
// keyspace by key prefix.
type redisDB struct {
	client *redis.Client
	pol    SecurityPolicy
	decor  SchemaDecor
}

func (r *redisDB) Kind() string { return "redis" }
func (r *redisDB) Close() error { return r.client.Close() }

var redisReadCmds = map[string]bool{
	"get": true, "mget": true, "strlen": true, "getrange": true, "substr": true, "exists": true,
	"type": true, "ttl": true, "pttl": true, "dump": false,
	"hget": true, "hgetall": true, "hkeys": true, "hvals": true, "hlen": true, "hmget": true, "hexists": true,
	"lrange": true, "llen": true, "lindex": true, "lpos": true,
	"smembers": true, "scard": true, "sismember": true, "srandmember": true, "sinter": true, "sunion": true, "sdiff": true,
	"zrange": true, "zrangebyscore": true, "zscore": true, "zcard": true, "zrank": true, "zrevrange": true, "zcount": true,
	"scan": true, "hscan": true, "sscan": true, "zscan": true, "keys": true, "dbsize": true, "randomkey": true,
	"object": true, "bitcount": true, "getbit": true, "memory": true, "ping": true,
}

var redisDenied = map[string]bool{
	"flushdb": true, "flushall": true, "config": true, "shutdown": true, "script": true, "eval": true,
	"evalsha": true, "debug": true, "swapdb": true, "migrate": true, "restore": true, "bgsave": true,
	"save": true, "bgrewriteaof": true, "slaveof": true, "replicaof": true, "acl": true, "cluster": true,
	"failover": true, "reset": true, "monitor": true,
}

func openRedis(ctx context.Context, cfg ConnConfig) (DB, error) {
	if err := guardEgress(cfg.Kind, cfg.DSN, cfg.Security); err != nil {
		return nil, err
	}
	opt, err := redis.ParseURL(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("dbaccess/redis: bad dsn: %w", err)
	}
	cli := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := cli.Ping(pctx).Err(); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("dbaccess/redis: ping: %w", err)
	}
	pol := cfg.Security
	for _, td := range cfg.Decor.Tables {
		pol.SensitiveColumns = append(pol.SensitiveColumns, td.Sensitive...)
	}
	return &redisDB{client: cli, pol: pol, decor: cfg.Decor}, nil
}

func (r *redisDB) Query(ctx context.Context, q string, _ ...any) (*Result, error) {
	parts := strings.Fields(q)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	cmd := strings.ToLower(parts[0])
	if redisDenied[cmd] {
		return nil, fmt.Errorf("redis: command %q is forbidden", cmd)
	}
	if r.pol.readOnly() && !redisReadCmds[cmd] {
		return nil, fmt.Errorf("read_only: command %q is not a read command", cmd)
	}
	ctx, cancel := context.WithTimeout(ctx, r.pol.timeout())
	defer cancel()

	args := make([]any, len(parts))
	for i, p := range parts {
		args[i] = p
	}
	v, err := r.client.Do(ctx, args...).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	return redisResult(v, r.pol), nil
}

func redisResult(v any, pol SecurityPolicy) *Result {
	sensitive := map[string]bool{}
	for _, c := range pol.SensitiveColumns {
		sensitive[strings.ToLower(c)] = true
	}
	max := pol.maxRows()
	res := &Result{Rows: []Row{}}
	switch x := v.(type) {
	case nil:
	case []any:
		for _, e := range x {
			if len(res.Rows) >= max {
				res.Truncated = true
				break
			}
			res.Rows = append(res.Rows, Row{"value": normalizeVal(e)})
		}
		res.Columns = []string{"value"}
	case map[any]any:
		row := Row{}
		for k, val := range x {
			key := fmt.Sprint(k)
			if sensitive[strings.ToLower(key)] {
				row[key] = "***"
			} else {
				row[key] = normalizeVal(val)
			}
		}
		res.Rows = append(res.Rows, row)
	default:
		res.Rows = append(res.Rows, Row{"value": normalizeVal(v)})
		res.Columns = []string{"value"}
	}
	res.RowCount = len(res.Rows)
	return res
}

func (r *redisDB) Schema(ctx context.Context) (*Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, r.pol.timeout())
	defer cancel()
	keys, _, err := r.client.Scan(ctx, 0, "*", 300).Result()
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	sample := map[string]string{}
	for _, k := range keys {
		p := k
		if i := strings.IndexByte(k, ':'); i >= 0 {
			p = k[:i]
		}
		counts[p]++
		if sample[p] == "" {
			sample[p] = k
		}
	}
	prefixes := make([]string, 0, len(counts))
	for p := range counts {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	cat := &Catalog{}
	for _, p := range prefixes {
		t := &TableInfo{Name: p, Description: fmt.Sprintf("~%d keys (sampled)", counts[p])}
		typ, _ := r.client.Type(ctx, sample[p]).Result()
		t.Columns = []ColumnInfo{{Name: sample[p], Type: typ}}
		if d, ok := r.decor.Tables[p]; ok {
			applyDecor(t, d)
		}
		cat.Tables = append(cat.Tables, *t)
	}
	return cat, nil
}
