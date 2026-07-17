package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore opens the pool, sizes it for concurrent leases + reads, and
// ensures the cursor table exists.
func NewPgStore(ctx context.Context, dsn string) (*PgStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConns < 32 {
		cfg.MaxConns = 32
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS digitorn_index_cursor (
		key text PRIMARY KEY,
		state bytea NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		pool.Close()
		return nil, err
	}
	return &PgStore{pool: pool}, nil
}

func (s *PgStore) Load(key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var b []byte
	err := s.pool.QueryRow(ctx, "SELECT state FROM digitorn_index_cursor WHERE key=$1", rowKey(key)).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func (s *PgStore) Save(key string, state []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO digitorn_index_cursor (key, state, updated_at) VALUES ($1,$2,now())
		 ON CONFLICT (key) DO UPDATE SET state=EXCLUDED.state, updated_at=now()`, rowKey(key), state)
	return err
}

// rowKey hashes the source key (which embeds NUL separators) to a Postgres-safe
// text primary key.
func rowKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// Acquire takes a cluster-wide session advisory lock for key on a dedicated
// pooled connection, held until release. ok=false means the lock is held by
// another replica (or the lock DB is unreachable) — the caller skips this run
// and the scheduler retries next tick, so it self-heals.
func (s *PgStore) Acquire(ctx context.Context, key string) (func(), bool) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return func() {}, false
	}
	id := advisoryKey(key)
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", id).Scan(&got); err != nil || !got {
		conn.Release()
		return func() {}, false
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", id)
		conn.Release()
	}, true
}

func (s *PgStore) Close() { s.pool.Close() }

func advisoryKey(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64())
}
