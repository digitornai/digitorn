package tokenizer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// modelTokenizerPath resolves the real tokenizer.json on this machine.
// Tests that need it skip when it is absent (CI without the model).
func modelTokenizerPath(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("DIGITORN_EMBED_MODEL_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		dir = filepath.Join(home, ".digitorn", "models", "paraphrase-multilingual-MiniLM-L12-v2")
	}
	p := filepath.Join(dir, "tokenizer.json")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("tokenizer.json not present (%s); run the embeddings model download", p)
	}
	return p
}

// TestUnigram_ConformanceVsHuggingFace locks the Go tokenizer to the
// exact ids HuggingFace tokenizers produces for the doc-default model.
// The expected ids were captured from `tokenizers` 0.22.2 loading the
// same tokenizer.json. A drift here means the embeddings would diverge
// from the reference model — this is the anti-divergence guard.
func TestUnigram_ConformanceVsHuggingFace(t *testing.T) {
	tok, err := NewUnigram(modelTokenizerPath(t))
	if err != nil {
		t.Fatalf("NewUnigram: %v", err)
	}
	cases := []struct {
		text string
		want []int64
	}{
		{"delete a file from disk", []int64{0, 154109, 10, 11435, 1295, 28338, 2}},
		{"supprimer un fichier", []int64{0, 15811, 78667, 51, 110267, 2}},
		{"free up space on my drive", []int64{0, 4092, 1257, 32628, 98, 759, 22648, 2}},
		{"rechercher dans la base de donnees", []int64{0, 22938, 42, 807, 21, 3647, 8, 17302, 90, 2}},
		{"こんにちは世界", []int64{0, 6, 192661, 3221, 2}},
	}
	for _, c := range cases {
		ids, mask, types, seq := tok.Encode(c.text)
		if !reflect.DeepEqual(ids, c.want) {
			t.Errorf("Encode(%q) ids = %v, want %v", c.text, ids, c.want)
		}
		if seq != len(c.want) {
			t.Errorf("Encode(%q) seq = %d, want %d", c.text, seq, len(c.want))
		}
		if len(mask) != seq || len(types) != seq {
			t.Errorf("Encode(%q) mask/types len = %d/%d, want %d", c.text, len(mask), len(types), seq)
		}
		for i := range mask {
			if mask[i] != 1 {
				t.Errorf("Encode(%q) mask[%d] = %d, want 1", c.text, i, mask[i])
			}
			if types[i] != 0 {
				t.Errorf("Encode(%q) types[%d] = %d, want 0", c.text, i, types[i])
			}
		}
	}
}

// TestUnigram_Wrapping checks the BOS/EOS framing and empty input.
func TestUnigram_Wrapping(t *testing.T) {
	tok, err := NewUnigram(modelTokenizerPath(t))
	if err != nil {
		t.Fatalf("NewUnigram: %v", err)
	}
	ids, _, _, seq := tok.Encode("")
	if seq != 2 || ids[0] != int64(tok.bosID) || ids[1] != int64(tok.eosID) {
		t.Fatalf("empty Encode = %v (seq %d), want [bos eos]", ids, seq)
	}
}

// TestUnigram_Truncation guards the maxSeq cap.
func TestUnigram_Truncation(t *testing.T) {
	tok, err := NewUnigram(modelTokenizerPath(t))
	if err != nil {
		t.Fatalf("NewUnigram: %v", err)
	}
	long := ""
	for i := 0; i < 2000; i++ {
		long += "word "
	}
	ids, mask, _, seq := tok.Encode(long)
	if seq > tok.maxSeq {
		t.Fatalf("seq %d exceeds maxSeq %d", seq, tok.maxSeq)
	}
	if len(mask) != seq {
		t.Fatalf("mask len %d != seq %d", len(mask), seq)
	}
	if ids[len(ids)-1] != int64(tok.eosID) {
		t.Fatalf("truncated sequence must still end with eos")
	}
}
