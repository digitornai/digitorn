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

type Mode string

const (

	ModeONNX Mode = "onnx"

	ModeDeterministic Mode = "deterministic"
	ModeAuto Mode = "auto"
)


type Manager struct {
	baseDir   string
	mode      Mode
	modelFile string
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


type modelEntry struct {
	once sync.Once
	be   backend.Backend
	err  error
}


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

func (m *Manager) DefaultModel(ctx context.Context) (string, int, error) {
	be, _, err := m.backendFor(ctx, "")
	if err != nil {
		return "", 0, err
	}
	return be.Model(), be.Dimension(), nil
}


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
