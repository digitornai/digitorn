package embeddings

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/digitornai/digitorn/internal/embeddings/backend"
	"github.com/digitornai/digitorn/internal/embeddings/loader"
	"github.com/digitornai/digitorn/internal/embeddings/models"
)

// Mode selects how the manager builds a backend for a model.
type Mode string

const (
	// ModeONNX requires the real ONNX runtime ; a model that fails to
	// load is an error (no silent semantic degradation).
	ModeONNX Mode = "onnx"
	// ModeDeterministic uses the pure-Go hashing fallback for every
	// model (CI / no-CGO builds). Vectors are NOT semantic.
	ModeDeterministic Mode = "deterministic"
	// ModeAuto tries ONNX and falls back to deterministic per model.
	ModeAuto Mode = "auto"
)

// Manager serves multiple embedding models from one worker process.
// Each model is loaded once on first use and cached ; loads for
// distinct models run concurrently and never block requests for an
// already-resident model. Routing is by the request's Model field
// (canonical id or shortcut, resolved via the models catalogue).
type Manager struct {
	baseDir   string
	mode      Mode
	modelFile string // "model.onnx" or "model_quantized.onnx"
	log       *slog.Logger

	mu      sync.Mutex
	entries map[string]*modelEntry

	rerankMu      sync.Mutex
	rerankEntries map[string]*rerankEntry
}

type rerankEntry struct {
	once sync.Once
	ce   backend.CrossEncoder
	err  error
}

// modelEntry guards one model's one-time load. once ensures a single
// loader runs even under concurrent first-use ; be/err hold the result.
type modelEntry struct {
	once sync.Once
	be   backend.Backend
	err  error
}

// NewManager builds a multi-model manager. baseDir is the parent of the
// per-model directories (default ~/.digitorn/models). quantized selects
// the int8 graph.
func NewManager(baseDir string, mode Mode, quantized bool, log *slog.Logger) *Manager {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".digitorn", "models")
	}
	if mode == "" {
		mode = ModeAuto
	}
	if log == nil {
		log = slog.Default()
	}
	mf := "model.onnx"
	if quantized {
		mf = "model_quantized.onnx"
	}
	return &Manager{
		baseDir:       baseDir,
		mode:          mode,
		modelFile:     mf,
		log:           log,
		entries:       make(map[string]*modelEntry),
		rerankEntries: make(map[string]*rerankEntry),
	}
}

// DefaultModel reports the id + dimension of the catalogue default,
// loading it if necessary. Used for Info and empty-input responses.
func (m *Manager) DefaultModel(ctx context.Context) (string, int, error) {
	be, _, err := m.backendFor(ctx, "")
	if err != nil {
		return "", 0, err
	}
	return be.Model(), be.Dimension(), nil
}

// Embed resolves the model, applies any role prefix, and returns one
// vector per input plus the resolved model id and dimension.
func (m *Manager) Embed(ctx context.Context, model, role string, inputs []string, normalize bool) ([][]float32, string, int, error) {
	be, spec, err := m.backendFor(ctx, model)
	if err != nil {
		return nil, "", 0, err
	}
	texts := applyPrefix(spec, role, inputs)
	vecs, err := be.Embed(ctx, texts, normalize)
	if err != nil {
		return nil, "", 0, err
	}
	return vecs, be.Model(), be.Dimension(), nil
}

// backendFor resolves a model id/shortcut to its (lazily loaded)
// backend + spec.
func (m *Manager) backendFor(ctx context.Context, model string) (backend.Backend, models.Spec, error) {
	spec, ok := models.Resolve(model)
	if !ok {
		return nil, models.Spec{}, fmt.Errorf("embeddings: unknown model %q", model)
	}
	m.mu.Lock()
	e := m.entries[spec.ID]
	if e == nil {
		e = &modelEntry{}
		m.entries[spec.ID] = e
	}
	m.mu.Unlock()

	e.once.Do(func() {
		e.be, e.err = m.load(ctx, spec)
	})
	if e.err != nil {
		return nil, models.Spec{}, e.err
	}
	return e.be, spec, nil
}

// load builds the backend for one spec per the manager's mode,
// downloading the model files first when an ONNX path is in play.
func (m *Manager) load(ctx context.Context, spec models.Spec) (backend.Backend, error) {
	if m.mode == ModeDeterministic {
		m.log.Info("embeddings: deterministic backend", "model", spec.ID, "dim", spec.Dim)
		return backend.NewDeterministicFor(spec), nil
	}

	modelDir := filepath.Join(m.baseDir, spec.ID)
	files := []loader.File{
		{Name: m.modelFile, URL: spec.ModelURL(m.modelFile)},
		{Name: "tokenizer.json", URL: spec.TokenizerURL()},
	}
	// The full graph may carry its weights in a sibling external-data
	// file (models >2 GB). The quantized graph is self-contained.
	if m.modelFile == "model.onnx" {
		for _, extra := range spec.ExtraFiles {
			files = append(files, loader.File{Name: extra, URL: spec.ExtraURL(extra)})
		}
	}
	if err := loader.Ensure(ctx, modelDir, files, m.log); err != nil {
		if m.mode == ModeAuto {
			m.log.Warn("embeddings: model fetch failed, using deterministic fallback", "model", spec.ID, "err", err.Error())
			return backend.NewDeterministicFor(spec), nil
		}
		return nil, fmt.Errorf("embeddings: fetch %s: %w", spec.ID, err)
	}

	be, err := backend.NewONNXFromSpec(modelDir, m.modelFile, spec)
	if err != nil {
		if m.mode == ModeAuto {
			m.log.Warn("embeddings: ONNX load failed, using deterministic fallback", "model", spec.ID, "err", err.Error())
			return backend.NewDeterministicFor(spec), nil
		}
		return nil, fmt.Errorf("embeddings: load %s: %w", spec.ID, err)
	}
	m.log.Info("embeddings: model loaded", "model", be.Model(), "dim", be.Dimension(), "pooling", spec.Pooling)
	return be, nil
}

// Rerank scores (query, doc) pairs with a cross-encoder, lazily loading
// the reranker model. Returns one score per doc (higher = more relevant)
// plus the resolved model id.
func (m *Manager) Rerank(ctx context.Context, model, query string, docs []string) ([]float32, string, error) {
	ce, err := m.crossEncoderFor(ctx, model)
	if err != nil {
		return nil, "", err
	}
	scores, err := ce.Rerank(ctx, query, docs)
	if err != nil {
		return nil, "", err
	}
	return scores, ce.Model(), nil
}

func (m *Manager) crossEncoderFor(ctx context.Context, model string) (backend.CrossEncoder, error) {
	spec, ok := models.ResolveReranker(model)
	if !ok {
		return nil, fmt.Errorf("embeddings: unknown reranker %q", model)
	}
	m.rerankMu.Lock()
	e := m.rerankEntries[spec.ID]
	if e == nil {
		e = &rerankEntry{}
		m.rerankEntries[spec.ID] = e
	}
	m.rerankMu.Unlock()

	e.once.Do(func() { e.ce, e.err = m.loadReranker(ctx, spec) })
	return e.ce, e.err
}

// loadReranker builds the cross-encoder for a spec. Rerankers always use
// the int8 graph (model_quantized.onnx) — fp32 cross-encoders are large
// and int8 reranking quality is excellent.
func (m *Manager) loadReranker(ctx context.Context, spec models.Spec) (backend.CrossEncoder, error) {
	if m.mode == ModeDeterministic {
		return backend.NewDeterministicCrossEncoder(spec), nil
	}
	const modelFile = "model_quantized.onnx"
	dir := filepath.Join(m.baseDir, spec.ID)
	files := []loader.File{
		{Name: modelFile, URL: spec.ModelURL(modelFile)},
		{Name: "tokenizer.json", URL: spec.TokenizerURL()},
	}
	if err := loader.Ensure(ctx, dir, files, m.log); err != nil {
		if m.mode == ModeAuto {
			m.log.Warn("embeddings: reranker fetch failed, no-op rerank", "model", spec.ID, "err", err.Error())
			return backend.NewDeterministicCrossEncoder(spec), nil
		}
		return nil, fmt.Errorf("embeddings: fetch reranker %s: %w", spec.ID, err)
	}
	ce, err := backend.NewONNXCrossEncoder(dir, modelFile, spec)
	if err != nil {
		if m.mode == ModeAuto {
			m.log.Warn("embeddings: reranker load failed, no-op rerank", "model", spec.ID, "err", err.Error())
			return backend.NewDeterministicCrossEncoder(spec), nil
		}
		return nil, fmt.Errorf("embeddings: load reranker %s: %w", spec.ID, err)
	}
	m.log.Info("embeddings: reranker loaded", "model", ce.Model())
	return ce, nil
}

// Close releases every loaded backend.
func (m *Manager) Close() error {
	m.mu.Lock()
	for _, e := range m.entries {
		if e.be != nil {
			_ = e.be.Close()
		}
	}
	m.mu.Unlock()
	m.rerankMu.Lock()
	for _, e := range m.rerankEntries {
		if e.ce != nil {
			_ = e.ce.Close()
		}
	}
	m.rerankMu.Unlock()
	return nil
}

// applyPrefix prepends the spec's retrieval prefix for the given role.
// No-op when the spec defines no prefix or the role is unset.
func applyPrefix(spec models.Spec, role string, inputs []string) []string {
	var prefix string
	switch role {
	case RoleQuery:
		prefix = spec.QueryPrefix
	case RoleDocument:
		prefix = spec.DocPrefix
	}
	if prefix == "" {
		return inputs
	}
	out := make([]string, len(inputs))
	for i, s := range inputs {
		out[i] = prefix + s
	}
	return out
}
