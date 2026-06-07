package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
)

// Tokenizer is the contract the ONNX backend uses : single-text Encode
// and sentence-pair EncodePair (for cross-encoder rerankers). Both the
// SentencePiece-Unigram and BERT-WordPiece families implement it.
type Tokenizer interface {
	Encode(text string) (ids, mask, types []int64, seqLen int)
	EncodePair(a, b string) (ids, mask, types []int64, seqLen int)
}

// Load reads a HuggingFace tokenizer.json and returns the right
// implementation for its model family (Unigram or WordPiece).
func Load(tokenizerJSONPath string) (Tokenizer, error) {
	raw, err := os.ReadFile(tokenizerJSONPath)
	if err != nil {
		return nil, err
	}
	var head struct {
		Model struct {
			Type string `json:"type"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("tokenizer: parse %s: %w", tokenizerJSONPath, err)
	}
	switch head.Model.Type {
	case "Unigram":
		return newUnigramFromBytes(raw)
	case "WordPiece":
		return newWordPieceFromBytes(raw)
	default:
		return nil, fmt.Errorf("tokenizer: unsupported model type %q", head.Model.Type)
	}
}
