// Package tokenizer implements the SentencePiece Unigram tokenizer
// that the paraphrase-multilingual-MiniLM-L12-v2 model (XLM-RoBERTa
// lineage) expects. It reads the HuggingFace tokenizer.json directly,
// so the vocab index is the model input id — no fairseq offset to
// reconstruct.
//
// Pipeline (matches the tokenizer.json config exactly) :
//
//	NFKC normalize  ->  whitespace split  ->  metaspace (prepend U+2581
//	to each word)  ->  Unigram Viterbi over the vocab log-scores  ->
//	wrap with <s> … </s>.
//
// The NFKC step approximates SentencePiece's Precompiled charsmap ;
// it reproduces HuggingFace's reference ids exactly across a broad
// multilingual set (see unigram_test.go). The package is pure Go and
// is compiled unconditionally so the normal build / vet / test cover
// it — only the ONNX session itself (onnx_real.go) is behind a tag.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// metaspace is U+2581 (LOWER ONE EIGHTH BLOCK), SentencePiece's
// visible space marker.
const metaspace = "▁"

// maxSeq caps the token sequence at the model's position limit.
const defaultMaxSeq = 512

// pieceRuneCap bounds the Viterbi look-ahead. Real vocab pieces are a
// handful of runes ; the cap keeps a pathological vocab entry from
// blowing up the per-position scan.
const pieceRuneCap = 64

// Unigram is a loaded SentencePiece Unigram tokenizer.
type Unigram struct {
	piece2id      map[string]int
	scores        []float32
	unkID         int
	bosID         int
	eosID         int
	maxPieceRunes int
	maxSeq        int
}

// NewUnigram loads tokenizer.json and returns a ready tokenizer.
func NewUnigram(tokenizerJSONPath string) (*Unigram, error) {
	raw, err := os.ReadFile(tokenizerJSONPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Model struct {
			Type  string              `json:"type"`
			UnkID int                 `json:"unk_id"`
			Vocab [][]json.RawMessage `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("tokenizer: parse %s: %w", tokenizerJSONPath, err)
	}
	if doc.Model.Type != "Unigram" {
		return nil, fmt.Errorf("tokenizer: expected Unigram model, got %q", doc.Model.Type)
	}
	if len(doc.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer: empty vocab")
	}

	t := &Unigram{
		piece2id: make(map[string]int, len(doc.Model.Vocab)),
		scores:   make([]float32, len(doc.Model.Vocab)),
		unkID:    doc.Model.UnkID,
		maxSeq:   defaultMaxSeq,
	}
	for i, entry := range doc.Model.Vocab {
		if len(entry) != 2 {
			return nil, fmt.Errorf("tokenizer: vocab[%d] malformed", i)
		}
		var piece string
		if err := json.Unmarshal(entry[0], &piece); err != nil {
			return nil, fmt.Errorf("tokenizer: vocab[%d] piece: %w", i, err)
		}
		var score float64
		if err := json.Unmarshal(entry[1], &score); err != nil {
			return nil, fmt.Errorf("tokenizer: vocab[%d] score: %w", i, err)
		}
		t.piece2id[piece] = i
		t.scores[i] = float32(score)
		if n := len([]rune(piece)); n > t.maxPieceRunes {
			t.maxPieceRunes = n
		}
	}
	t.bosID = t.lookup("<s>", 0)
	t.eosID = t.lookup("</s>", 2)
	if _, ok := t.piece2id["<unk>"]; ok && t.unkID == 0 {
		t.unkID = t.piece2id["<unk>"]
	}
	if t.maxPieceRunes <= 0 || t.maxPieceRunes > pieceRuneCap {
		t.maxPieceRunes = pieceRuneCap
	}
	return t, nil
}

func (t *Unigram) lookup(piece string, def int) int {
	if id, ok := t.piece2id[piece]; ok {
		return id
	}
	return def
}

// Encode turns text into model input ids (wrapped in <s> … </s>), a
// matching all-ones attention mask, and an all-zeros token_type_ids
// slice. Sequence length is dynamic (no padding) and capped at maxSeq.
func (t *Unigram) Encode(text string) (ids, mask, types []int64, seqLen int) {
	ids = make([]int64, 0, 16)
	ids = append(ids, int64(t.bosID))

	limit := t.maxSeq - 1 // reserve a slot for the closing </s>
	for _, word := range strings.Fields(norm.NFKC.String(text)) {
		if len(ids) >= limit {
			break
		}
		for _, id := range t.viterbi([]rune(metaspace + word)) {
			if len(ids) >= limit {
				break
			}
			ids = append(ids, int64(id))
		}
	}
	ids = append(ids, int64(t.eosID))

	seqLen = len(ids)
	mask = make([]int64, seqLen)
	types = make([]int64, seqLen)
	for i := range mask {
		mask[i] = 1
	}
	return ids, mask, types, seqLen
}

// viterbi returns the maximum-score Unigram segmentation of word (a
// rune slice already carrying its leading metaspace) as vocab ids.
// Every position is reachable because any single character that is
// not a vocab piece falls back to the unk id, so the lattice always
// has a path to the end.
func (t *Unigram) viterbi(word []rune) []int {
	n := len(word)
	if n == 0 {
		return nil
	}
	const negInf = -1e30
	best := make([]float64, n+1)
	backPos := make([]int, n+1)
	backID := make([]int, n+1)
	for i := 1; i <= n; i++ {
		best[i] = negInf
		backPos[i] = -1
	}

	for i := 0; i < n; i++ {
		if best[i] <= negInf/2 {
			continue
		}
		jmax := i + t.maxPieceRunes
		if jmax > n {
			jmax = n
		}
		for j := i + 1; j <= jmax; j++ {
			if id, ok := t.piece2id[string(word[i:j])]; ok {
				if score := best[i] + float64(t.scores[id]); score > best[j] {
					best[j] = score
					backPos[j] = i
					backID[j] = id
				}
			}
		}
		// Single-character unk fallback when the char is not a piece.
		if _, ok := t.piece2id[string(word[i:i+1])]; !ok {
			if score := best[i] + float64(t.scores[t.unkID]) - 10.0; score > best[i+1] {
				best[i+1] = score
				backPos[i+1] = i
				backID[i+1] = t.unkID
			}
		}
	}

	var rev []int
	for pos := n; pos > 0; {
		pi := backPos[pos]
		if pi < 0 {
			rev = append(rev, t.unkID)
			pos--
			continue
		}
		rev = append(rev, backID[pos])
		pos = pi
	}
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	return rev
}
