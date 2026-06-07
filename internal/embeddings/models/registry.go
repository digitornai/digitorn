// Package models is the catalogue of embedding models the worker can
// serve. It is pure data (no CGO, no ONNX) so every build — daemon,
// tests, cross-compile — links it. The ONNX backend reads a Spec to
// know the output dimension, the pooling strategy, the tokenizer
// family and any retrieval prefixes ; the loader reads it to know
// which files to fetch and from where.
//
// The shortcuts mirror the Python RAG module's `embedding_model`
// field (minilm-l12, bge-m3, bge-small, nomic-v1.5, jina-v3) so an
// app YAML written against the old documentation resolves to the
// same model here.
package models

import "strings"

// Pooling is how token hidden states collapse into one sentence vector.
type Pooling string

const (
	// PoolingMean averages token states weighted by the attention mask
	// (sentence-transformers default ; minilm, mpnet, nomic).
	PoolingMean Pooling = "mean"
	// PoolingCLS takes the first token's state (BGE retrieval default).
	PoolingCLS Pooling = "cls"
)

// Tokenizer is the sub-word algorithm the model's vocabulary uses.
// The two families need different decoders ; a model loaded with the
// wrong one produces garbage vectors, so the backend refuses a family
// it cannot tokenise rather than silently degrade.
type Tokenizer string

const (
	// Unigram is SentencePiece-Unigram (XLM-RoBERTa lineage : minilm,
	// bge-m3, jina-v3, multilingual mpnet). Supported today.
	Unigram Tokenizer = "unigram"
	// WordPiece is BERT WordPiece (bge-small-en, nomic). Not wired yet.
	WordPiece Tokenizer = "wordpiece"
)

// Spec fully describes one servable model.
type Spec struct {
	// ID is the canonical on-disk identifier — the model directory
	// name and the value echoed back in EmbedResponse.Model.
	ID string
	// Dim is the output vector length (auto-checked against the graph).
	Dim int
	// Pooling collapses token states into the sentence vector.
	Pooling Pooling
	// Tokenizer is the sub-word family the vocab uses.
	Tokenizer Tokenizer
	// QueryPrefix / DocPrefix are prepended before embedding when the
	// request carries the matching Role. Required by some models
	// (nomic : "search_query:" / "search_document:") ; empty for most.
	QueryPrefix string
	DocPrefix   string
	// Repo is the HuggingFace repo serving the ONNX graph + tokenizer.
	Repo string
	// ModelSubpath is the path of the graph within the repo
	// (e.g. "onnx/model.onnx"). Empty defaults to "onnx/model.onnx".
	ModelSubpath string
	// TokenizerSubpath is the path of tokenizer.json within the repo.
	// Empty defaults to "tokenizer.json".
	TokenizerSubpath string
	// ExtraFiles are sibling files the full graph needs at load time —
	// the ONNX external-data weights (model.onnx_data) for models >2 GB.
	// Fetched into the model dir next to model.onnx so onnxruntime
	// resolves them automatically. Only used for the full graph, not
	// the self-contained quantized variant.
	ExtraFiles []string
}

// Default is the doc-mandated model — the value an empty request
// resolves to, identical to the historic single-model worker.
const Default = "paraphrase-multilingual-MiniLM-L12-v2"

// catalogue is the canonical id → Spec table.
var catalogue = map[string]Spec{
	"paraphrase-multilingual-MiniLM-L12-v2": {
		ID:        "paraphrase-multilingual-MiniLM-L12-v2",
		Dim:       384,
		Pooling:   PoolingMean,
		Tokenizer: Unigram,
		Repo:      "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2",
	},
	"bge-m3": {
		ID:         "bge-m3",
		Dim:        1024,
		Pooling:    PoolingCLS,
		Tokenizer:  Unigram, // XLM-RoBERTa-large lineage
		Repo:       "Xenova/bge-m3",
		ExtraFiles: []string{"model.onnx_data"}, // fp32 weights (external data)
	},
	"paraphrase-multilingual-mpnet-base-v2": {
		ID:        "paraphrase-multilingual-mpnet-base-v2",
		Dim:       768,
		Pooling:   PoolingMean,
		Tokenizer: Unigram, // XLM-RoBERTa lineage
		Repo:      "Xenova/paraphrase-multilingual-mpnet-base-v2",
	},
	"bge-small-en-v1.5": {
		ID:        "bge-small-en-v1.5",
		Dim:       384,
		Pooling:   PoolingCLS,
		Tokenizer: WordPiece,
		Repo:      "Xenova/bge-small-en-v1.5",
	},
	"nomic-embed-text-v1.5": {
		ID:          "nomic-embed-text-v1.5",
		Dim:         768,
		Pooling:     PoolingMean,
		Tokenizer:   WordPiece,
		QueryPrefix: "search_query: ",
		DocPrefix:   "search_document: ",
		Repo:        "nomic-ai/nomic-embed-text-v1.5",
	},
	"jina-embeddings-v2-base-code": {
		ID:        "jina-embeddings-v2-base-code",
		Dim:       768,
		Pooling:   PoolingMean,
		Tokenizer: WordPiece,
		Repo:      "Xenova/jina-embeddings-v2-base-code",
	},
}

// rerankers catalogues cross-encoder reranking models (Unigram-family
// only for now ; same tokenizer as our embedding path).
var rerankers = map[string]Spec{
	"bge-reranker-base": {
		ID:        "bge-reranker-base",
		Pooling:   PoolingCLS, // unused by cross-encoders ; kept for shape
		Tokenizer: Unigram,    // XLM-RoBERTa lineage
		Repo:      "Xenova/bge-reranker-base",
	},
}

var rerankerShortcuts = map[string]string{
	"bge-reranker":      "bge-reranker-base",
	"bge-reranker-base": "bge-reranker-base",
}

// DefaultReranker is the model `reranker: true` resolves to.
const DefaultReranker = "bge-reranker-base"

// ResolveReranker maps a reranker id/shortcut to its Spec ; empty → default.
func ResolveReranker(id string) (Spec, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultReranker
	}
	if canon, ok := rerankerShortcuts[strings.ToLower(id)]; ok {
		id = canon
	}
	s, ok := rerankers[id]
	return s, ok
}

// shortcuts maps the Python `embedding_model` aliases to canonical ids.
var shortcuts = map[string]string{
	"minilm-l12":  "paraphrase-multilingual-MiniLM-L12-v2",
	"minilm":      "paraphrase-multilingual-MiniLM-L12-v2",
	"bge-m3":      "bge-m3",
	"mpnet":       "paraphrase-multilingual-mpnet-base-v2",
	"bge-small":   "bge-small-en-v1.5",
	"nomic-v1.5":  "nomic-embed-text-v1.5",
	"nomic":       "nomic-embed-text-v1.5",
	"jina-code":   "jina-embeddings-v2-base-code",
	"code":        "jina-embeddings-v2-base-code",
}

// Resolve maps a shortcut or canonical id to its Spec. An empty id
// resolves to the default model. The boolean is false for an unknown
// id so the caller can return a clear error instead of a wrong model.
func Resolve(id string) (Spec, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		s := catalogue[Default]
		return s, true
	}
	if canon, ok := shortcuts[strings.ToLower(id)]; ok {
		id = canon
	}
	s, ok := catalogue[id]
	return s, ok
}

// modelSubpath returns the in-repo path of the graph file, honouring an
// explicit override and defaulting to onnx/<file>.
func (s Spec) modelSubpath(modelFile string) string {
	if s.ModelSubpath != "" {
		return s.ModelSubpath
	}
	return "onnx/" + modelFile
}

// tokenizerSubpath returns the in-repo path of tokenizer.json.
func (s Spec) tokenizerSubpath() string {
	if s.TokenizerSubpath != "" {
		return s.TokenizerSubpath
	}
	return "tokenizer.json"
}

// URL builds an absolute HuggingFace resolve URL for an in-repo path.
func (s Spec) URL(subpath string) string {
	return "https://huggingface.co/" + s.Repo + "/resolve/main/" + subpath
}

// ModelURL is the download URL for the named graph file.
func (s Spec) ModelURL(modelFile string) string { return s.URL(s.modelSubpath(modelFile)) }

// TokenizerURL is the download URL for tokenizer.json.
func (s Spec) TokenizerURL() string { return s.URL(s.tokenizerSubpath()) }

// ExtraURL is the download URL for a sibling file (e.g. model.onnx_data)
// living in the same repo directory as the full graph.
func (s Spec) ExtraURL(name string) string {
	dir := s.modelSubpath("model.onnx")
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i+1]
	} else {
		dir = ""
	}
	return s.URL(dir + name)
}
