package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgvectorBackend struct {
	pool *pgxpool.Pool
}

func newPgvectorBackend(cfg Backend) (VectorBackend, error) {
	dsn := strings.TrimSpace(cfg.DSN)
	if dsn == "" {
		dsn = strings.TrimSpace(cfg.URL)
	}
	if dsn == "" {
		return nil, fmt.Errorf("rag/pgvector: a dsn (or url) is required")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, fmt.Errorf("rag/pgvector: connect: %w", err)
	}
	if _, err := pool.Exec(context.Background(), "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		pool.Close()
		return nil, fmt.Errorf("rag/pgvector: enable vector extension: %w", err)
	}
	return &pgvectorBackend{pool: pool}, nil
}

var pgSafeName = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func pgTable(kb string) string {
	return "kb_" + strings.ToLower(pgSafeName.ReplaceAllString(kb, "_"))
}

func (p *pgvectorBackend) ident(kb string) string {
	return pgx.Identifier{pgTable(kb)}.Sanitize()
}

func (p *pgvectorBackend) EnsureKB(ctx context.Context, kb string, dim int) error {
	t := p.ident(kb)
	q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id text PRIMARY KEY,
		text text,
		source text,
		chunk int,
		meta jsonb DEFAULT '{}'::jsonb,
		embedding vector(%d)
	)`, t, dim)
	if _, err := p.pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("rag/pgvector: create %s: %w", t, err)
	}
	idx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding vector_cosine_ops)`,
		pgx.Identifier{pgTable(kb) + "_emb_idx"}.Sanitize(), t)
	_, _ = p.pool.Exec(ctx, idx)
	return nil
}

func (p *pgvectorBackend) DeleteKB(ctx context.Context, kb string) error {
	if _, err := p.pool.Exec(ctx, "DROP TABLE IF EXISTS "+p.ident(kb)); err != nil {
		return fmt.Errorf("rag/pgvector: drop %q: %w", kb, err)
	}
	return nil
}

func (p *pgvectorBackend) ListKBs(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name LIKE 'kb\_%' ESCAPE '\'`)
	if err != nil {
		return nil, fmt.Errorf("rag/pgvector: list: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimPrefix(name, "kb_"))
	}
	return out, rows.Err()
}

func (p *pgvectorBackend) CountKB(ctx context.Context, kb string) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx, "SELECT count(*) FROM "+p.ident(kb)).Scan(&n); err != nil {
		return 0, fmt.Errorf("rag/pgvector: count %q: %w", kb, err)
	}
	return n, nil
}

func (p *pgvectorBackend) KBInfo(ctx context.Context, kb string) (KBStats, error) {
	var count int
	if err := p.pool.QueryRow(ctx, "SELECT count(*) FROM "+p.ident(kb)).Scan(&count); err != nil {
		return KBStats{Exists: false}, nil
	}
	var dim int
	_ = p.pool.QueryRow(ctx, "SELECT vector_dims(embedding) FROM "+p.ident(kb)+" LIMIT 1").Scan(&dim)
	return KBStats{Exists: true, Dim: dim, Count: count}, nil
}

func (p *pgvectorBackend) Upsert(ctx context.Context, kb string, docs []Document) error {
	t := p.ident(kb)
	q := fmt.Sprintf(`INSERT INTO %s (id, text, source, chunk, meta, embedding)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6::vector)
		ON CONFLICT (id) DO UPDATE SET
			text = EXCLUDED.text, source = EXCLUDED.source,
			chunk = EXCLUDED.chunk, meta = EXCLUDED.meta, embedding = EXCLUDED.embedding`, t)
	batch := &pgx.Batch{}
	for _, d := range docs {
		meta := d.Meta
		if meta == nil {
			meta = map[string]any{}
		}
		mj, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("rag/pgvector: marshal meta: %w", err)
		}
		batch.Queue(q, d.ID, d.Text, d.Source, d.Chunk, string(mj), vecLiteral(d.Vector))
	}
	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range docs {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("rag/pgvector: upsert %q: %w", kb, err)
		}
	}
	return nil
}

func (p *pgvectorBackend) Search(ctx context.Context, kb string, vector []float32, topK int, filter Filter) ([]SearchHit, error) {
	where, args := pgWhere(filter)
	q := fmt.Sprintf(`SELECT id, text, source, chunk, meta, 1 - (embedding <=> $1::vector) AS score
		FROM %s %s ORDER BY embedding <=> $1::vector LIMIT %d`, p.ident(kb), where, topK)
	allArgs := append([]any{vecLiteral(vector)}, args...)
	rows, err := p.pool.Query(ctx, q, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("rag/pgvector: search %q: %w", kb, err)
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		d, score, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, SearchHit{Document: d, Score: score})
	}
	return hits, rows.Err()
}

func (p *pgvectorBackend) DeleteBySource(ctx context.Context, kb, source string) error {
	if _, err := p.pool.Exec(ctx, "DELETE FROM "+p.ident(kb)+" WHERE source = $1", source); err != nil {
		return fmt.Errorf("rag/pgvector: delete source %q in %q: %w", source, kb, err)
	}
	return nil
}

func (p *pgvectorBackend) Scan(ctx context.Context, kb string) ([]Document, error) {
	rows, err := p.pool.Query(ctx, "SELECT id, text, source, chunk, meta, 0::float8 FROM "+p.ident(kb))
	if err != nil {
		return nil, fmt.Errorf("rag/pgvector: scan %q: %w", kb, err)
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, _, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *pgvectorBackend) Close() error {
	p.pool.Close()
	return nil
}

func scanDoc(rows pgx.Rows) (Document, float32, error) {
	var d Document
	var metaRaw []byte
	var score float64
	if err := rows.Scan(&d.ID, &d.Text, &d.Source, &d.Chunk, &metaRaw, &score); err != nil {
		return Document{}, 0, fmt.Errorf("rag/pgvector: scan row: %w", err)
	}
	if len(metaRaw) > 0 {
		var mm map[string]any
		if json.Unmarshal(metaRaw, &mm) == nil && len(mm) > 0 {
			d.Meta = mm
		}
	}
	return d, float32(score), nil
}

func pgWhere(f Filter) (string, []any) {
	if f.Empty() {
		return "", nil
	}
	var clauses []string
	var args []any
	n := 2 // $1 is the query vector
	for field, vals := range f.Must {
		if len(vals) == 0 {
			continue
		}
		safe := pgSafeName.ReplaceAllString(field, "_")
		clauses = append(clauses, fmt.Sprintf(
			`(meta->>'%s' = ANY($%d::text[]) OR (jsonb_typeof(meta->'%s') = 'array' AND meta->'%s' ?| $%d::text[]))`,
			safe, n, safe, safe, n))
		args = append(args, vals)
		n++
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func vecLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
