package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// qdrantBackend connects to the app's Qdrant server over gRPC and
// implements VectorBackend. A knowledge base maps to a Qdrant
// collection ; chunk text + citation metadata ride in the point payload.
type qdrantBackend struct {
	cli *qdrant.Client
}

// newQdrantBackend dials the Qdrant server declared in the app's backend
// config. Empty URL defaults to localhost:6334 (the gRPC port).
func newQdrantBackend(cfg Backend) (VectorBackend, error) {
	host, port, useTLS, err := parseQdrantAddr(cfg.URL)
	if err != nil {
		return nil, err
	}
	cli, err := qdrant.NewClient(&qdrant.Config{
		Host:                   host,
		Port:                   port,
		APIKey:                 cfg.APIKey,
		UseTLS:                 useTLS,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("rag/qdrant: connect %s:%d: %w", host, port, err)
	}
	return &qdrantBackend{cli: cli}, nil
}

func (q *qdrantBackend) EnsureKB(ctx context.Context, kb string, dim int) error {
	cols, err := q.cli.ListCollections(ctx)
	if err != nil {
		return fmt.Errorf("rag/qdrant: list: %w", err)
	}
	for _, c := range cols {
		if c == kb {
			return nil
		}
	}
	if err := q.cli.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: kb,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(dim),
			Distance: qdrant.Distance_Cosine,
		}),
	}); err != nil {
		return fmt.Errorf("rag/qdrant: create %q: %w", kb, err)
	}
	return nil
}

func (q *qdrantBackend) DeleteKB(ctx context.Context, kb string) error {
	if err := q.cli.DeleteCollection(ctx, kb); err != nil {
		return fmt.Errorf("rag/qdrant: delete %q: %w", kb, err)
	}
	return nil
}

func (q *qdrantBackend) ListKBs(ctx context.Context) ([]string, error) {
	cols, err := q.cli.ListCollections(ctx)
	if err != nil {
		return nil, fmt.Errorf("rag/qdrant: list: %w", err)
	}
	return cols, nil
}

func (q *qdrantBackend) CountKB(ctx context.Context, kb string) (int, error) {
	n, err := q.cli.Count(ctx, &qdrant.CountPoints{CollectionName: kb})
	if err != nil {
		return 0, fmt.Errorf("rag/qdrant: count %q: %w", kb, err)
	}
	return int(n), nil
}

func (q *qdrantBackend) Upsert(ctx context.Context, kb string, docs []Document) error {
	points := make([]*qdrant.PointStruct, len(docs))
	for i, d := range docs {
		points[i] = &qdrant.PointStruct{
			Id:      qdrant.NewID(d.ID),
			Vectors: qdrant.NewVectors(d.Vector...),
			Payload: map[string]*qdrant.Value{
				"text":   qdrant.NewValueString(d.Text),
				"source": qdrant.NewValueString(d.Source),
				"chunk":  qdrant.NewValueInt(int64(d.Chunk)),
			},
		}
	}
	if _, err := q.cli.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: kb,
		Points:         points,
	}); err != nil {
		return fmt.Errorf("rag/qdrant: upsert %q: %w", kb, err)
	}
	return nil
}

func (q *qdrantBackend) Search(ctx context.Context, kb string, vector []float32, topK int) ([]SearchHit, error) {
	limit := uint64(topK)
	pts, err := q.cli.Query(ctx, &qdrant.QueryPoints{
		CollectionName: kb,
		Query:          qdrant.NewQuery(vector...),
		Limit:          &limit,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("rag/qdrant: query %q: %w", kb, err)
	}
	hits := make([]SearchHit, 0, len(pts))
	for _, p := range pts {
		pl := p.GetPayload()
		hits = append(hits, SearchHit{
			Document: Document{
				ID:     p.GetId().GetUuid(),
				Text:   pl["text"].GetStringValue(),
				Source: pl["source"].GetStringValue(),
				Chunk:  int(pl["chunk"].GetIntegerValue()),
			},
			Score: p.GetScore(),
		})
	}
	return hits, nil
}

func (q *qdrantBackend) DeleteBySource(ctx context.Context, kb, source string) error {
	_, err := q.cli.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: kb,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewMatchKeyword("source", source)},
		}),
	})
	if err != nil {
		return fmt.Errorf("rag/qdrant: delete source %q in %q: %w", source, kb, err)
	}
	return nil
}

func (q *qdrantBackend) Scan(ctx context.Context, kb string) ([]Document, error) {
	limit := uint32(256)
	it := q.cli.ScrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: kb,
		Limit:          &limit,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	var out []Document
	for {
		page, err := it.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rag/qdrant: scan %q: %w", kb, err)
		}
		for _, p := range page {
			pl := p.GetPayload()
			out = append(out, Document{
				ID:     p.GetId().GetUuid(),
				Text:   pl["text"].GetStringValue(),
				Source: pl["source"].GetStringValue(),
				Chunk:  int(pl["chunk"].GetIntegerValue()),
			})
		}
	}
	return out, nil
}

func (q *qdrantBackend) Close() error { return q.cli.Close() }

// parseQdrantAddr extracts host/port/TLS from a backend URL. The Go
// client speaks gRPC (default port 6334) ; a URL that names the REST
// port 6333 is mapped to 6334 so old REST-style configs connect
// transparently. Empty URL → localhost:6334.
func parseQdrantAddr(raw string) (host string, port int, useTLS bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "localhost", 6334, false, nil
	}
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, perr := url.Parse(s)
	if perr != nil {
		return "", 0, false, fmt.Errorf("rag/qdrant: bad url %q: %w", raw, perr)
	}
	host = u.Hostname()
	if host == "" {
		host = "localhost"
	}
	useTLS = u.Scheme == "https"
	port = 6334
	if ps := u.Port(); ps != "" {
		if p, e := strconv.Atoi(ps); e == nil {
			port = p
		}
	}
	if port == 6333 {
		port = 6334
	}
	return host, port, useTLS, nil
}

// newBackend builds the VectorBackend for a parsed Config. Qdrant is the
// only connector wired in Phase 1 ; others (pgvector/chroma/…) land in a
// later phase behind this same switch.
func newBackend(cfg Config) (VectorBackend, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Backend.Type)) {
	case "", "qdrant":
		return newQdrantBackend(cfg.Backend)
	default:
		return nil, fmt.Errorf("rag: backend %q not supported yet (qdrant only in this phase)", cfg.Backend.Type)
	}
}
