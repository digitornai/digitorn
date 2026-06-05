//go:build onnx

package backend

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func onnxModelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("DIGITORN_EMBED_MODEL_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		dir = filepath.Join(home, ".digitorn", "models", "paraphrase-multilingual-MiniLM-L12-v2")
	}
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err != nil {
		t.Skipf("model.onnx not present (%s)", dir)
	}
	return dir
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestONNX_RealSemantics proves the full pipeline (Unigram tokenizer →
// ONNX forward pass → mean-pool) yields vectors whose cosine geometry
// reflects MEANING, not surface tokens: a paraphrase ranks far above an
// unrelated sentence, across languages.
func TestONNX_RealSemantics(t *testing.T) {
	be, err := NewONNX(onnxModelDir(t))
	if err != nil {
		t.Fatalf("NewONNX: %v", err)
	}
	defer be.Close()

	if be.Model() != "paraphrase-multilingual-MiniLM-L12-v2" {
		t.Errorf("Model() = %q", be.Model())
	}
	if be.Dimension() != 384 {
		t.Errorf("Dimension() = %d, want 384", be.Dimension())
	}

	texts := []string{
		"delete a file from disk",        // 0 anchor
		"remove a document from storage", // 1 paraphrase (no shared content words)
		"supprimer un fichier du disque", // 2 French translation
		"the weather is sunny today",     // 3 unrelated
	}
	vecs, err := be.Embed(context.Background(), texts, true)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if len(v) != be.Dimension() {
			t.Fatalf("vec[%d] len = %d, want %d", i, len(v), be.Dimension())
		}
	}

	paraphrase := cosine(vecs[0], vecs[1])
	translation := cosine(vecs[0], vecs[2])
	unrelated := cosine(vecs[0], vecs[3])
	t.Logf("cos(anchor,paraphrase)=%.3f cos(anchor,french)=%.3f cos(anchor,unrelated)=%.3f",
		paraphrase, translation, unrelated)

	if paraphrase <= unrelated {
		t.Errorf("paraphrase (%.3f) should outrank unrelated (%.3f)", paraphrase, unrelated)
	}
	if translation <= unrelated {
		t.Errorf("cross-lingual match (%.3f) should outrank unrelated (%.3f)", translation, unrelated)
	}
	if paraphrase < 0.5 {
		t.Errorf("paraphrase similarity %.3f unexpectedly low — pipeline likely wrong", paraphrase)
	}
}

// TestONNX_Quantized_Semantics proves the int8 model_quantized.onnx
// (~4x smaller) preserves semantic geometry : paraphrase + cross-lingual
// still rank far above an unrelated sentence. Skips when the quantized
// file isn't downloaded.
func TestONNX_Quantized_Semantics(t *testing.T) {
	dir := onnxModelDir(t)
	if _, err := os.Stat(filepath.Join(dir, "model_quantized.onnx")); err != nil {
		t.Skipf("model_quantized.onnx not present (%s)", dir)
	}
	be, err := NewONNXWithFile(dir, "model_quantized.onnx")
	if err != nil {
		t.Fatalf("NewONNXWithFile(quantized): %v", err)
	}
	defer be.Close()
	if be.Dimension() != 384 {
		t.Errorf("Dimension() = %d, want 384", be.Dimension())
	}
	texts := []string{
		"delete a file from disk",
		"remove a document from storage",
		"supprimer un fichier du disque",
		"the weather is sunny today",
	}
	vecs, err := be.Embed(context.Background(), texts, true)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	paraphrase := cosine(vecs[0], vecs[1])
	translation := cosine(vecs[0], vecs[2])
	unrelated := cosine(vecs[0], vecs[3])
	t.Logf("[quantized] paraphrase=%.3f french=%.3f unrelated=%.3f", paraphrase, translation, unrelated)
	if paraphrase <= unrelated || translation <= unrelated {
		t.Errorf("quantized lost semantic ordering: para=%.3f fr=%.3f unrel=%.3f", paraphrase, translation, unrelated)
	}
	if paraphrase < 0.4 {
		t.Errorf("quantized paraphrase similarity %.3f too low", paraphrase)
	}
}

// TestGPUOrder_PlatformChain locks the device→EP-candidate mapping so a
// regression can't silently drop GPU selection. Pure logic, no device.
func TestGPUOrder_PlatformChain(t *testing.T) {
	cases := []struct {
		device string
		want   []string
	}{
		{"cpu", nil},
		{"cuda", []string{"cuda"}},
		{"directml", []string{"directml"}},
		{"dml", []string{"directml"}},
		{"coreml", []string{"coreml"}},
		{"bogus", nil},
	}
	for _, c := range cases {
		got := gpuOrderFor(c.device)
		if len(got) != len(c.want) {
			t.Errorf("gpuOrderFor(%q) = %v, want %v", c.device, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("gpuOrderFor(%q)[%d] = %q, want %q", c.device, i, got[i], c.want[i])
			}
		}
	}
	// auto must yield a non-empty GPU chain on every supported platform.
	if len(gpuOrderFor("auto")) == 0 {
		t.Errorf("gpuOrderFor(auto) is empty on %s — GPU never tried", runtime.GOOS)
	}
	// cpu request must produce nil options (onnxruntime default).
	if o, ep := buildSessionOptions("cpu"); o != nil || ep != "cpu" {
		t.Errorf("buildSessionOptions(cpu) = (%v,%q), want (nil,cpu)", o, ep)
	}
}
