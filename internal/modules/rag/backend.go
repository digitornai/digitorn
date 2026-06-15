package rag

import "context"

// Document is one stored unit — a chunk plus its vector and the metadata
// a citation needs (source + position within that source).
type Document struct {
	ID     string
	Vector []float32
	Text   string
	Source string
	Chunk  int
	Meta   map[string]any
}

// SearchHit is a retrieved Document with its similarity score (cosine,
// higher is closer).
type SearchHit struct {
	Document
	Score float32
}

// KBStats describes an existing collection : its vector dimension and how
// many documents it holds. Exists is false when the collection is absent.
type KBStats struct {
	Exists bool
	Dim    int
	Count  int
}

// VectorBackend is the app's vector store behind a uniform interface, so
// the connector is swappable (Qdrant now ; pgvector/chroma/pinecone/…
// later) without touching the engine. A knowledge base maps to one named
// collection inside the backend.
type VectorBackend interface {
	// EnsureKB makes sure a collection named kb exists with the given
	// vector dimension. Idempotent ; a dimension mismatch is an error.
	EnsureKB(ctx context.Context, kb string, dim int) error
	// DeleteKB removes a collection and all its vectors.
	DeleteKB(ctx context.Context, kb string) error
	// ListKBs returns the collection names the backend holds.
	ListKBs(ctx context.Context) ([]string, error)
	// CountKB returns how many vectors a collection holds.
	CountKB(ctx context.Context, kb string) (int, error)
	// KBInfo describes a collection that may already exist in the backend —
	// a pre-built index the app did not create here — reporting its vector
	// dimension and document count so a KB can be discovered and queried
	// without reindexing. Exists is false when the collection is absent.
	KBInfo(ctx context.Context, kb string) (KBStats, error)
	// Upsert writes documents (by ID) into a collection.
	Upsert(ctx context.Context, kb string, docs []Document) error
	// DeleteBySource removes every chunk whose payload source == source,
	// enabling a clean re-sync of one document/file.
	DeleteBySource(ctx context.Context, kb, source string) error
	// Search returns the topK nearest documents to vector in a collection,
	// constrained to documents whose metadata satisfies filter (applied AT
	// the vector layer — ACL filter-first). An empty filter matches all.
	Search(ctx context.Context, kb string, vector []float32, topK int, filter Filter) ([]SearchHit, error)
	// Scan returns every document (text + citation metadata, no vector) in a
	// collection — used to rebuild the in-memory keyword index on cold start.
	Scan(ctx context.Context, kb string) ([]Document, error)
	// Close releases any held connection.
	Close() error
}

// KeywordSearcher is an optional backend capability : keyword (BM25) search run
// server-side, so the engine need not pin an in-memory keyword index of the
// whole corpus in RAM. Backends with native full-text search (Elasticsearch)
// implement it ; others fall back to the in-process BM25. Hits must carry their
// Document + score and already satisfy filter.
type KeywordSearcher interface {
	KeywordSearch(ctx context.Context, kb, query string, topK int, filter Filter) ([]SearchHit, error)
}
