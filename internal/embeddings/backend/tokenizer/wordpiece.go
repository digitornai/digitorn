// WordPiece implements the BERT WordPiece tokenizer (used by code
// embedders like jina-code, by bge-small-en/nomic, and by ms-marco
// cross-encoders). Reads the HuggingFace tokenizer.json directly :
// vocab + unk/continuation + the BertNormalizer lowercase/strip-accents
// flags + [CLS]/[SEP]. Pairs carry BERT segment ids (0 for A, 1 for B).
package tokenizer

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

type WordPiece struct {
	vocab         map[string]int
	unkID         int
	contPrefix    string
	maxInputChars int
	clsID, sepID  int
	lowercase     bool
	stripAccents  bool
	maxSeq        int
}

func newWordPieceFromBytes(raw []byte) (*WordPiece, error) {
	var doc struct {
		Normalizer struct {
			Lowercase    *bool `json:"lowercase"`
			StripAccents *bool `json:"strip_accents"`
		} `json:"normalizer"`
		Model struct {
			Type                    string         `json:"type"`
			UnkToken                string         `json:"unk_token"`
			ContinuingSubwordPrefix string         `json:"continuing_subword_prefix"`
			MaxInputCharsPerWord    int            `json:"max_input_chars_per_word"`
			Vocab                   map[string]int `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("tokenizer: parse wordpiece: %w", err)
	}
	if doc.Model.Type != "WordPiece" {
		return nil, fmt.Errorf("tokenizer: expected WordPiece, got %q", doc.Model.Type)
	}
	if len(doc.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer: empty wordpiece vocab")
	}
	w := &WordPiece{
		vocab:         doc.Model.Vocab,
		contPrefix:    doc.Model.ContinuingSubwordPrefix,
		maxInputChars: doc.Model.MaxInputCharsPerWord,
		maxSeq:        defaultMaxSeq,
		lowercase:     doc.Normalizer.Lowercase == nil || *doc.Normalizer.Lowercase,
		stripAccents:  doc.Normalizer.StripAccents != nil && *doc.Normalizer.StripAccents,
	}
	if w.contPrefix == "" {
		w.contPrefix = "##"
	}
	if w.maxInputChars <= 0 {
		w.maxInputChars = 100
	}
	unk := doc.Model.UnkToken
	if unk == "" {
		unk = "[UNK]"
	}
	w.unkID = w.vocab[unk]
	w.clsID = w.vocab["[CLS]"]
	w.sepID = w.vocab["[SEP]"]
	return w, nil
}

func (w *WordPiece) Encode(text string) (ids, mask, types []int64, seqLen int) {
	ids = append(ids, int64(w.clsID))
	ids = w.pieces(text, ids, w.maxSeq-1)
	ids = append(ids, int64(w.sepID))
	return finalize(ids)
}

// EncodePair builds [CLS] A [SEP] B [SEP] with BERT segment ids (A=0, B=1).
func (w *WordPiece) EncodePair(a, b string) (ids, mask, types []int64, seqLen int) {
	ids = append(ids, int64(w.clsID))
	ids = w.pieces(a, ids, w.maxSeq-3)
	ids = append(ids, int64(w.sepID))
	segA := len(ids) // [CLS] A [SEP] are segment 0
	ids = w.pieces(b, ids, w.maxSeq-1)
	ids = append(ids, int64(w.sepID))

	seqLen = len(ids)
	mask = make([]int64, seqLen)
	types = make([]int64, seqLen)
	for i := range mask {
		mask[i] = 1
		if i >= segA {
			types[i] = 1
		}
	}
	return ids, mask, types, seqLen
}

// pieces normalizes + basic-tokenizes text and appends WordPiece ids,
// stopping at limit.
func (w *WordPiece) pieces(text string, ids []int64, limit int) []int64 {
	for _, tok := range w.basicTokens(text) {
		if len(ids) >= limit {
			break
		}
		for _, id := range w.wordpiece(tok) {
			if len(ids) >= limit {
				break
			}
			ids = append(ids, int64(id))
		}
	}
	return ids
}

func (w *WordPiece) basicTokens(text string) []string {
	if w.lowercase {
		text = strings.ToLower(text)
	}
	if w.stripAccents {
		text = stripAccents(text)
	}
	var out []string
	for _, field := range strings.FieldsFunc(text, unicode.IsSpace) {
		out = append(out, splitPunct(field)...)
	}
	return out
}

// wordpiece greedily matches the longest vocab subword from the front ;
// an unmatchable token (or one over the char cap) becomes [UNK].
func (w *WordPiece) wordpiece(token string) []int {
	runes := []rune(token)
	if len(runes) > w.maxInputChars {
		return []int{w.unkID}
	}
	var out []int
	start := 0
	for start < len(runes) {
		end := len(runes)
		matched := -1
		for end > start {
			sub := string(runes[start:end])
			if start > 0 {
				sub = w.contPrefix + sub
			}
			if id, ok := w.vocab[sub]; ok {
				matched = id
				break
			}
			end--
		}
		if matched < 0 {
			return []int{w.unkID}
		}
		out = append(out, matched)
		start = end
	}
	return out
}

// splitPunct isolates each punctuation rune as its own token (BERT rule).
func splitPunct(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if isPunct(r) {
			flush()
			out = append(out, string(r))
		} else {
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func isPunct(r rune) bool {
	if (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~') {
		return true
	}
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// stripAccents removes combining marks (NFD then drop Mn).
func stripAccents(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
