package models

import "testing"

func TestResolve_ShortcutsAndDefault(t *testing.T) {
	cases := []struct {
		in      string
		wantID  string
		wantDim int
		wantOK  bool
	}{
		{"", "paraphrase-multilingual-MiniLM-L12-v2", 384, true},
		{"minilm-l12", "paraphrase-multilingual-MiniLM-L12-v2", 384, true},
		{"MiniLM", "paraphrase-multilingual-MiniLM-L12-v2", 384, true},
		{"bge-m3", "bge-m3", 1024, true},
		{"mpnet", "paraphrase-multilingual-mpnet-base-v2", 768, true},
		{"nomic", "nomic-embed-text-v1.5", 768, true},
		{"paraphrase-multilingual-MiniLM-L12-v2", "paraphrase-multilingual-MiniLM-L12-v2", 384, true},
		{"does-not-exist", "", 0, false},
	}
	for _, c := range cases {
		spec, ok := Resolve(c.in)
		if ok != c.wantOK {
			t.Errorf("Resolve(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if spec.ID != c.wantID {
			t.Errorf("Resolve(%q).ID = %q, want %q", c.in, spec.ID, c.wantID)
		}
		if spec.Dim != c.wantDim {
			t.Errorf("Resolve(%q).Dim = %d, want %d", c.in, spec.Dim, c.wantDim)
		}
	}
}

func TestSpec_PoolingAndTokenizer(t *testing.T) {
	minilm, _ := Resolve("minilm-l12")
	if minilm.Pooling != PoolingMean || minilm.Tokenizer != Unigram {
		t.Errorf("minilm pooling=%q tok=%q", minilm.Pooling, minilm.Tokenizer)
	}
	bge, _ := Resolve("bge-m3")
	if bge.Pooling != PoolingCLS || bge.Tokenizer != Unigram {
		t.Errorf("bge-m3 pooling=%q tok=%q", bge.Pooling, bge.Tokenizer)
	}
	// nomic carries required retrieval prefixes.
	nomic, _ := Resolve("nomic")
	if nomic.QueryPrefix == "" || nomic.DocPrefix == "" {
		t.Errorf("nomic prefixes missing: q=%q d=%q", nomic.QueryPrefix, nomic.DocPrefix)
	}
}

func TestSpec_URLs(t *testing.T) {
	bge, _ := Resolve("bge-m3")
	if got := bge.ModelURL("model.onnx"); got != "https://huggingface.co/Xenova/bge-m3/resolve/main/onnx/model.onnx" {
		t.Errorf("ModelURL = %q", got)
	}
	if got := bge.TokenizerURL(); got != "https://huggingface.co/Xenova/bge-m3/resolve/main/tokenizer.json" {
		t.Errorf("TokenizerURL = %q", got)
	}
	// external-data sibling resolves to the same onnx/ dir.
	if got := bge.ExtraURL("model.onnx_data"); got != "https://huggingface.co/Xenova/bge-m3/resolve/main/onnx/model.onnx_data" {
		t.Errorf("ExtraURL = %q", got)
	}
	if len(bge.ExtraFiles) != 1 || bge.ExtraFiles[0] != "model.onnx_data" {
		t.Errorf("ExtraFiles = %v", bge.ExtraFiles)
	}
}
