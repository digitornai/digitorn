package rag

import "context"

type Document struct {
	ID     string
	Vector []float32
	Text   string
	Source string
	Chunk  int
	Meta   map[string]any
}

type SearchHit struct {
	Document
	Score float32
}

type KBStats struct {
	Exists bool
	Dim    int
	Count  int
}

type VectorBackend interface {
	EnsureKB(ctx context.Context, kb string, dim int) error
	DeleteKB(ctx context.Context, kb string) error
	ListKBs(ctx context.Context) ([]string, error)
	CountKB(ctx context.Context, kb string) (int, error)
	KBInfo(ctx context.Context, kb string) (KBStats, error)
	Upsert(ctx context.Context, kb string, docs []Document) error
	DeleteBySource(ctx context.Context, kb, source string) error
	Search(ctx context.Context, kb string, vector []float32, topK int, filter Filter) ([]SearchHit, error)
	Scan(ctx context.Context, kb string) ([]Document, error)
	Close() error
}

type KeywordSearcher interface {
	KeywordSearch(ctx context.Context, kb, query string, topK int, filter Filter) ([]SearchHit, error)
}
