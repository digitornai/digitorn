package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// esBackend implements VectorBackend on Elasticsearch (8.x dense_vector + kNN).
// A knowledge base maps to one index ; chunk text + citation metadata ride in
// the document, with arbitrary filter metadata in a flattened field so ACL
// term filters apply inside the kNN query. The indexer's ragSink writes here
// and the engine reads here — same store, no extra plumbing.
type esBackend struct {
	base       string
	apiKey     string
	user, pass string
	cli        *http.Client
}

func newElasticBackend(cfg Backend) (VectorBackend, error) {
	base := strings.TrimSpace(cfg.URL)
	if base == "" {
		base = "http://localhost:9200"
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("rag/elastic: bad url %q: %w", base, err)
	}
	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
		u.User = nil
	}
	return &esBackend{
		base:   strings.TrimRight(u.String(), "/"),
		apiKey: cfg.APIKey,
		user:   user, pass: pass,
		cli: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

var esNonName = regexp.MustCompile(`[^a-z0-9_-]`)

func esIndex(kb string) string {
	return "kb_" + esNonName.ReplaceAllString(strings.ToLower(kb), "_")
}

func (b *esBackend) req(ctx context.Context, method, path, contentType string, raw []byte) (int, []byte, error) {
	var body io.Reader
	if raw != nil {
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "ApiKey "+b.apiKey)
	} else if b.user != "" {
		req.SetBasicAuth(b.user, b.pass)
	}
	resp, err := b.cli.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

func (b *esBackend) json(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	return b.req(ctx, method, path, "application/json", raw)
}

func (b *esBackend) EnsureKB(ctx context.Context, kb string, dim int) error {
	idx := esIndex(kb)
	if st, _, _ := b.req(ctx, http.MethodHead, "/"+idx, "", nil); st == 200 {
		return nil
	}
	mapping := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"vector": map[string]any{"type": "dense_vector", "dims": dim, "index": true, "similarity": "cosine"},
				"text":   map[string]any{"type": "text"},
				"source": map[string]any{"type": "keyword"},
				"chunk":  map[string]any{"type": "integer"},
				"meta":   map[string]any{"type": "flattened"},
			},
		},
	}
	st, body, err := b.json(ctx, http.MethodPut, "/"+idx, mapping)
	if err != nil {
		return fmt.Errorf("rag/elastic: create %q: %w", idx, err)
	}
	if st >= 300 && !bytes.Contains(body, []byte("resource_already_exists")) {
		return fmt.Errorf("rag/elastic: create %q: %s", idx, body)
	}
	return nil
}

func (b *esBackend) DeleteKB(ctx context.Context, kb string) error {
	st, body, err := b.req(ctx, http.MethodDelete, "/"+esIndex(kb), "", nil)
	if err != nil {
		return err
	}
	if st >= 300 && st != 404 {
		return fmt.Errorf("rag/elastic: delete %q: %s", kb, body)
	}
	return nil
}

func (b *esBackend) ListKBs(ctx context.Context) ([]string, error) {
	st, body, err := b.req(ctx, http.MethodGet, "/_cat/indices/kb_*?format=json&h=index", "", nil)
	if err != nil {
		return nil, err
	}
	if st >= 300 {
		return nil, fmt.Errorf("rag/elastic: list: %s", body)
	}
	var rows []struct {
		Index string `json:"index"`
	}
	_ = json.Unmarshal(body, &rows)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.TrimPrefix(r.Index, "kb_"))
	}
	return out, nil
}

func (b *esBackend) CountKB(ctx context.Context, kb string) (int, error) {
	st, body, err := b.json(ctx, http.MethodGet, "/"+esIndex(kb)+"/_count", nil)
	if err != nil {
		return 0, err
	}
	if st >= 300 {
		return 0, fmt.Errorf("rag/elastic: count %q: %s", kb, body)
	}
	var r struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Count, nil
}

func (b *esBackend) KBInfo(ctx context.Context, kb string) (KBStats, error) {
	idx := esIndex(kb)
	if st, _, _ := b.req(ctx, http.MethodHead, "/"+idx, "", nil); st != 200 {
		return KBStats{Exists: false}, nil
	}
	st, body, err := b.json(ctx, http.MethodGet, "/"+idx+"/_mapping", nil)
	if err != nil || st >= 300 {
		return KBStats{Exists: true}, nil
	}
	dim := 0
	var m map[string]struct {
		Mappings struct {
			Properties struct {
				Vector struct {
					Dims int `json:"dims"`
				} `json:"vector"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if json.Unmarshal(body, &m) == nil {
		if v, ok := m[idx]; ok {
			dim = v.Mappings.Properties.Vector.Dims
		}
	}
	count, _ := b.CountKB(ctx, kb)
	return KBStats{Exists: true, Dim: dim, Count: count}, nil
}

func (b *esBackend) Upsert(ctx context.Context, kb string, docs []Document) error {
	idx := esIndex(kb)
	var buf bytes.Buffer
	for _, d := range docs {
		meta := d.Meta
		if meta == nil {
			meta = map[string]any{}
		}
		action, _ := json.Marshal(map[string]any{"index": map[string]any{"_index": idx, "_id": d.ID}})
		doc, _ := json.Marshal(map[string]any{
			"vector": d.Vector, "text": d.Text, "source": d.Source, "chunk": d.Chunk, "meta": meta,
		})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
	}
	st, body, err := b.req(ctx, http.MethodPost, "/_bulk?refresh=wait_for", "application/x-ndjson", buf.Bytes())
	if err != nil {
		return err
	}
	if st >= 300 {
		return fmt.Errorf("rag/elastic: bulk: %s", body)
	}
	var r struct {
		Errors bool `json:"errors"`
	}
	if json.Unmarshal(body, &r) == nil && r.Errors {
		return fmt.Errorf("rag/elastic: bulk had item errors: %s", truncate(body, 400))
	}
	return nil
}

func (b *esBackend) DeleteBySource(ctx context.Context, kb, source string) error {
	q := map[string]any{"query": map[string]any{"term": map[string]any{"source": source}}}
	st, body, err := b.json(ctx, http.MethodPost, "/"+esIndex(kb)+"/_delete_by_query?refresh=true&conflicts=proceed", q)
	if err != nil {
		return err
	}
	if st >= 300 && st != 404 {
		return fmt.Errorf("rag/elastic: delete_by_query: %s", body)
	}
	return nil
}

func (b *esBackend) Search(ctx context.Context, kb string, vector []float32, topK int, filter Filter) ([]SearchHit, error) {
	if len(vector) == 0 {
		return nil, fmt.Errorf("rag/elastic: empty query vector")
	}
	nc := max(topK*10, 100)
	knn := map[string]any{"field": "vector", "query_vector": vector, "k": topK, "num_candidates": nc}
	if fs := esFilters(filter); len(fs) > 0 {
		knn["filter"] = fs
	}
	q := map[string]any{"knn": knn, "size": topK, "_source": []string{"text", "source", "chunk", "meta"}}
	st, body, err := b.json(ctx, http.MethodPost, "/"+esIndex(kb)+"/_search", q)
	if err != nil {
		return nil, err
	}
	if st >= 300 {
		return nil, fmt.Errorf("rag/elastic: search: %s", body)
	}
	return parseHits(body)
}

// KeywordSearch runs Elasticsearch's native BM25 (a match query on the text
// field) with the metadata filter applied server-side — so the engine never
// holds the corpus in RAM for the keyword side of hybrid retrieval.
func (b *esBackend) KeywordSearch(ctx context.Context, kb, query string, topK int, filter Filter) ([]SearchHit, error) {
	boolq := map[string]any{
		"must": []any{map[string]any{"match": map[string]any{"text": query}}},
	}
	if fs := esFilters(filter); len(fs) > 0 {
		boolq["filter"] = fs
	}
	q := map[string]any{
		"size":    topK,
		"_source": []string{"text", "source", "chunk", "meta"},
		"query":   map[string]any{"bool": boolq},
	}
	st, body, err := b.json(ctx, http.MethodPost, "/"+esIndex(kb)+"/_search", q)
	if err != nil {
		return nil, err
	}
	if st >= 300 {
		return nil, fmt.Errorf("rag/elastic: keyword search: %s", body)
	}
	return parseHits(body)
}

func (b *esBackend) Scan(ctx context.Context, kb string) ([]Document, error) {
	idx := esIndex(kb)
	q := map[string]any{"size": 1000, "query": map[string]any{"match_all": map[string]any{}}, "_source": []string{"text", "source", "chunk", "meta"}}
	st, body, err := b.json(ctx, http.MethodPost, "/"+idx+"/_search?scroll=1m", q)
	if err != nil {
		return nil, err
	}
	if st == 404 {
		return nil, nil
	}
	if st >= 300 {
		return nil, fmt.Errorf("rag/elastic: scan: %s", body)
	}

	var out []Document
	for {
		hits, scrollID, err := parseScroll(body)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
		if len(hits) == 0 || scrollID == "" {
			if scrollID != "" {
				_, _, _ = b.json(ctx, http.MethodDelete, "/_search/scroll", map[string]any{"scroll_id": scrollID})
			}
			break
		}
		_, body, err = b.json(ctx, http.MethodPost, "/_search/scroll", map[string]any{"scroll": "1m", "scroll_id": scrollID})
		if err != nil {
			break
		}
	}
	return out, nil
}

func (b *esBackend) Close() error { b.cli.CloseIdleConnections(); return nil }

// esFilters turns a metadata Filter into Elasticsearch term filters (one terms
// clause per field — OR within a field, AND across fields).
func esFilters(f Filter) []map[string]any {
	if f.Empty() {
		return nil
	}
	out := make([]map[string]any, 0, len(f.Must))
	for field, values := range f.Must {
		out = append(out, map[string]any{"terms": map[string]any{"meta." + field: values}})
	}
	return out
}

type esHit struct {
	Score  float32 `json:"_score"`
	Source struct {
		Text   string         `json:"text"`
		Source string         `json:"source"`
		Chunk  int            `json:"chunk"`
		Meta   map[string]any `json:"meta"`
	} `json:"_source"`
	ID string `json:"_id"`
}

func parseHits(body []byte) ([]SearchHit, error) {
	var r struct {
		Hits struct {
			Hits []esHit `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(r.Hits.Hits))
	for _, h := range r.Hits.Hits {
		out = append(out, SearchHit{
			Document: Document{ID: h.ID, Text: h.Source.Text, Source: h.Source.Source, Chunk: h.Source.Chunk, Meta: h.Source.Meta},
			Score:    h.Score,
		})
	}
	return out, nil
}

func parseScroll(body []byte) ([]Document, string, error) {
	var r struct {
		ScrollID string `json:"_scroll_id"`
		Hits     struct {
			Hits []esHit `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, "", err
	}
	out := make([]Document, 0, len(r.Hits.Hits))
	for _, h := range r.Hits.Hits {
		out = append(out, Document{ID: h.ID, Text: h.Source.Text, Source: h.Source.Source, Chunk: h.Source.Chunk, Meta: h.Source.Meta})
	}
	return out, r.ScrollID, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
