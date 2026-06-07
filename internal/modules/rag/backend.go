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
	// Upsert writes documents (by ID) into a collection.
	Upsert(ctx context.Context, kb string, docs []Document) error
	// DeleteBySource removes every chunk whose payload source == source,
	// enabling a clean re-sync of one document/file.
	DeleteBySource(ctx context.Context, kb, source string) error
	// Search returns the topK nearest documents to vector in a collection.
	Search(ctx context.Context, kb string, vector []float32, topK int) ([]SearchHit, error)
	// Scan returns every document (text + citation metadata, no vector) in a
	// collection — used to rebuild the in-memory keyword index on cold start.
	Scan(ctx context.Context, kb string) ([]Document, error)
	// Close releases any held connection.
	Close() error
}
