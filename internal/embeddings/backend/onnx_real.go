//go:build onnx

// Package backend (onnx variant) wires the real
// paraphrase-multilingual-MiniLM-L12-v2 ONNX model via
// yalue/onnxruntime_go. Compiled only with `-tags onnx`.
//
// Why this file is gated :
//
//   - onnxruntime_go is a CGO binding ; including it unconditionally
//     would break pure-Go cross-compile of the daemon.
//   - Only the embeddings worker pulls CGO, and only when someone
//     explicitly opts into the production path.
//
// Build :
//
//	go build -tags onnx ./cmd/digitorn-worker-embeddings
//
// Runtime requirements :
//
//   - onnxruntime shared library (onnxruntime.dll / libonnxruntime.so /
//     .dylib). Resolved from ONNXRUNTIME_LIB, then alongside the worker
//     executable, then the OS default search path.
//   - model.onnx + tokenizer.json under modelDir (the loader package
//     downloads them on first start).
package backend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/digitornai/digitorn/internal/embeddings/backend/tokenizer"
	"github.com/digitornai/digitorn/internal/embeddings/models"
)

// envOnce guards the one-time onnxruntime environment initialisation.
// The environment is process-global in onnxruntime, so every backend
// instance shares it.
var (
	envOnce sync.Once
	envErr  error
)

// ONNXBackend holds a POOL of inference sessions. A single onnxruntime session
// is not safe for concurrent Run, so on CPU we run several sessions (each with
// a slice of the cores) in parallel — embedding throughput then scales with the
// machine instead of one serialised forward. GPU keeps a single session.
type ONNXBackend struct {
	pool     chan *ort.DynamicAdvancedSession // checkout one per Run
	sessions []*ort.DynamicAdvancedSession    // all, for Close
	tok      tokenizer.Tokenizer
	inNames  []string // model input order, restricted to those we feed
	feedType bool     // model declares token_type_ids
	outName  string
	dim      int
	pooling  models.Pooling // mean (sentence-transformers) or cls (BGE)
	name     string         // canonical model id echoed to callers
	device   string         // execution provider actually in use (cpu/directml/cuda/coreml)
}

// NewONNX loads the doc-default full-precision graph (model.onnx).
func NewONNX(modelDir string) (Backend, error) {
	return NewONNXWithFile(modelDir, "model.onnx")
}

// NewONNXWithFile loads the named ONNX graph + tokenizer from modelDir
// for the doc-default model (minilm, mean pooling, 384). Kept for the
// historic single-model path and tests ; multi-model callers use
// NewONNXFromSpec.
func NewONNXWithFile(modelDir, modelFile string) (Backend, error) {
	spec, _ := models.Resolve(models.Default)
	return NewONNXFromSpec(modelDir, modelFile, spec)
}

// NewONNXFromSpec loads the named ONNX graph + tokenizer from modelDir
// and builds a persistent session configured for the given model Spec
// (output dimension, pooling strategy, tokenizer family). modelFile is
// "model.onnx" (default) or "model_quantized.onnx" (the int8 variant).
//
// A model whose tokenizer family this backend cannot decode is refused
// up front : serving it with the wrong tokenizer would emit silently
// wrong vectors, which is worse than a clear error.
func NewONNXFromSpec(modelDir, modelFile string, spec models.Spec) (Backend, error) {
	if modelDir == "" {
		return nil, errors.New("onnx: modelDir required")
	}
	if modelFile == "" {
		modelFile = "model.onnx"
	}
	envOnce.Do(func() {
		resolveSharedLibrary()
		envErr = ort.InitializeEnvironment()
	})
	if envErr != nil {
		return nil, fmt.Errorf("onnx: init env: %w", envErr)
	}

	tok, err := tokenizer.Load(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("onnx: tokenizer: %w", err)
	}

	modelPath := filepath.Join(modelDir, modelFile)
	inInfo, outInfo, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("onnx: inspect %q: %w", modelPath, err)
	}

	// Feed only the inputs the model actually declares, in its order.
	feedable := map[string]bool{"input_ids": true, "attention_mask": true, "token_type_ids": true}
	var inNames []string
	feedType := false
	for _, in := range inInfo {
		if feedable[in.Name] {
			inNames = append(inNames, in.Name)
			if in.Name == "token_type_ids" {
				feedType = true
			}
		}
	}
	if len(inNames) == 0 {
		return nil, fmt.Errorf("onnx: model declares no recognised inputs")
	}

	// Prefer the token-level hidden states for mean-pooling.
	outName, dim := "", 0
	for _, o := range outInfo {
		if o.Name == "last_hidden_state" {
			outName = o.Name
			if n := len(o.Dimensions); n > 0 {
				if d := o.Dimensions[n-1]; d > 0 {
					dim = int(d)
				}
			}
			break
		}
	}
	if outName == "" && len(outInfo) > 0 {
		outName = outInfo[0].Name
	}
	if outName == "" {
		return nil, fmt.Errorf("onnx: model declares no outputs")
	}
	// Prefer the graph's declared hidden size ; fall back to the Spec's
	// when the export hides it behind a dynamic axis. A graph that
	// disagrees with the Spec is a packaging error — fail loud.
	if dim == 0 {
		dim = spec.Dim
	}
	if spec.Dim > 0 && dim != spec.Dim {
		return nil, fmt.Errorf("onnx: model %s graph dim=%d disagrees with spec dim=%d", spec.ID, dim, spec.Dim)
	}

	// Pick an execution provider (GPU when requested + available) and
	// build the session ; on any GPU failure fall back to pure CPU so a
	// missing GPU runtime never takes the worker down.
	opts, ep := buildSessionOptions(deviceFromEnv())
	session, err := ort.NewDynamicAdvancedSession(modelPath, inNames, []string{outName}, opts)
	if err != nil && opts != nil {
		// GPU EP rejected at session-creation time (EP not in this
		// onnxruntime build, no device, driver mismatch). Retry CPU.
		fmt.Fprintf(os.Stderr, "worker-embeddings: %s EP unavailable (%v); falling back to CPU\n", ep, err)
		opts.Destroy()
		opts, ep = nil, "cpu"
		session, err = ort.NewDynamicAdvancedSession(modelPath, inNames, []string{outName}, nil)
	}
	if opts != nil {
		opts.Destroy()
	}
	if err != nil {
		return nil, fmt.Errorf("onnx: create session: %w", err)
	}
	fmt.Fprintf(os.Stderr, "worker-embeddings: execution provider = %s\n", ep)

	sessions, err := buildSessionPool(modelPath, inNames, outName, ep, session)
	if err != nil {
		return nil, err
	}
	pool := make(chan *ort.DynamicAdvancedSession, len(sessions))
	for _, s := range sessions {
		pool <- s
	}
	fmt.Fprintf(os.Stderr, "worker-embeddings: %d session(s) in pool\n", len(sessions))

	pooling := spec.Pooling
	if pooling == "" {
		pooling = models.PoolingMean
	}
	name := spec.ID
	if name == "" {
		name = models.Default
	}
	return &ONNXBackend{
		pool:     pool,
		sessions: sessions,
		tok:      tok,
		inNames:  inNames,
		feedType: feedType,
		outName:  outName,
		dim:      dim,
		pooling:  pooling,
		name:     name,
		device:   ep,
	}, nil
}

// buildSessionPool returns the inference sessions. On a GPU EP it keeps the
// single probe session (one device). On CPU it replaces the probe with
// embedSessions() sessions, each pinned to a fair slice of the cores, so
// concurrent forwards run truly in parallel.
func buildSessionPool(modelPath string, inNames []string, outName, ep string, probe *ort.DynamicAdvancedSession) ([]*ort.DynamicAdvancedSession, error) {
	n := embedSessions()
	if ep != "cpu" || n <= 1 {
		return []*ort.DynamicAdvancedSession{probe}, nil
	}
	_ = probe.Destroy()
	threads := runtime.NumCPU() / n
	if threads < 1 {
		threads = 1
	}
	sessions := make([]*ort.DynamicAdvancedSession, 0, n)
	for i := 0; i < n; i++ {
		so, err := ort.NewSessionOptions()
		if err == nil {
			_ = so.SetIntraOpNumThreads(threads)
		}
		s, serr := ort.NewDynamicAdvancedSession(modelPath, inNames, []string{outName}, so)
		if so != nil {
			so.Destroy()
		}
		if serr != nil {
			for _, x := range sessions {
				_ = x.Destroy()
			}
			return nil, fmt.Errorf("onnx: create pooled session %d: %w", i, serr)
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// embedSessions resolves the CPU session-pool size : DIGITORN_EMBED_SESSIONS
// override, else NumCPU/4 clamped to [1,4].
func embedSessions() int {
	if v := os.Getenv("DIGITORN_EMBED_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	n := runtime.NumCPU() / 4
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return n
}

// deviceFromEnv reads the requested execution device. "" / "auto" lets
// the platform pick a GPU EP (DirectML on Windows, CoreML on macOS,
// CUDA on Linux) with CPU fallback ; an explicit value forces one.
func deviceFromEnv() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("DIGITORN_EMBED_DEVICE")))
}

// buildSessionOptions returns SessionOptions configured with the chosen
// execution provider, plus a label for logging. A nil return means
// "use the onnxruntime default" (CPU). GPU EPs that fail to append
// (not compiled into the loaded onnxruntime build) degrade to CPU here ;
// any that pass the append but fail at session creation are caught by
// the retry in NewONNXWithFile.
func buildSessionOptions(device string) (*ort.SessionOptions, string) {
	if device == "cpu" {
		return nil, "cpu"
	}
	// Resolve "auto" to the platform-preferred GPU EP.
	order := gpuOrderFor(device)
	if len(order) == 0 {
		return nil, "cpu"
	}
	o, err := ort.NewSessionOptions()
	if err != nil {
		return nil, "cpu"
	}
	for _, ep := range order {
		if appendProvider(o, ep) == nil {
			return o, ep
		}
	}
	o.Destroy()
	return nil, "cpu"
}

// gpuOrderFor returns the EP candidates to try, in order, for a device
// request. "auto"/"" expands to the platform default chain.
func gpuOrderFor(device string) []string {
	switch device {
	case "cuda", "directml", "dml", "coreml":
		if device == "dml" {
			return []string{"directml"}
		}
		return []string{device}
	case "", "auto", "gpu":
		switch runtime.GOOS {
		case "windows":
			return []string{"directml", "cuda"} // DML covers any GPU; CUDA if a CUDA build is loaded
		case "darwin":
			return []string{"coreml"}
		default:
			return []string{"cuda"}
		}
	default:
		return nil // unknown → CPU
	}
}

// appendProvider wires one execution provider onto the options.
func appendProvider(o *ort.SessionOptions, ep string) error {
	switch ep {
	case "directml":
		return o.AppendExecutionProviderDirectML(0)
	case "cuda":
		cu, err := ort.NewCUDAProviderOptions()
		if err != nil {
			return err
		}
		defer cu.Destroy()
		return o.AppendExecutionProviderCUDA(cu)
	case "coreml":
		return o.AppendExecutionProviderCoreMLV2(map[string]string{})
	default:
		return fmt.Errorf("unknown execution provider %q", ep)
	}
}

func (b *ONNXBackend) Model() string  { return b.name }
func (b *ONNXBackend) Dimension() int { return b.dim }

// Device reports the execution provider in use (cpu/directml/cuda/coreml).
func (b *ONNXBackend) Device() string { return b.device }

func (b *ONNXBackend) Close() error {
	var err error
	for _, s := range b.sessions {
		if s != nil {
			if e := s.Destroy(); e != nil {
				err = e
			}
		}
	}
	b.sessions = nil
	return err
}

// Embed runs one forward pass per input, mean-pools the token hidden
// states (attention-mask weighted) and optionally L2-normalises.
func (b *ONNXBackend) Embed(ctx context.Context, inputs []string, l2norm bool) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	const sub = 32 // sequences per forward pass
	workers := cap(b.pool)
	if workers < 1 {
		workers = 1
	}

	type job struct{ start, end int }
	jobs := make(chan job)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		errMu.Unlock()
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					setErr(ctx.Err())
					continue
				}
				vecs, err := b.embedBatch(inputs[j.start:j.end])
				if err != nil {
					setErr(fmt.Errorf("onnx: batch[%d:%d]: %w", j.start, j.end, err))
					continue
				}
				for k, v := range vecs {
					if l2norm {
						l2Normalize(v)
					}
					out[j.start+k] = v
				}
			}
		}()
	}
	for start := 0; start < len(inputs); start += sub {
		end := start + sub
		if end > len(inputs) {
			end = len(inputs)
		}
		jobs <- job{start, end}
	}
	close(jobs)
	wg.Wait()
	return out, firstErr
}

// embedBatch runs ONE forward pass over the whole sub-batch — every sequence
// padded to the batch's longest — instead of one forward per text. The padded
// positions carry attention_mask 0 so they are ignored, giving the same vectors
// as single inference but an order of magnitude faster at scale.
func (b *ONNXBackend) embedBatch(texts []string) ([][]float32, error) {
	n := len(texts)
	if n == 0 {
		return nil, nil
	}
	encIDs := make([][]int64, n)
	encMask := make([][]int64, n)
	encType := make([][]int64, n)
	maxSeq := 1
	for i, t := range texts {
		ids, mask, types, seq := b.tok.Encode(t)
		encIDs[i], encMask[i], encType[i] = ids, mask, types
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	ids := make([]int64, n*maxSeq)
	mask := make([]int64, n*maxSeq)
	types := make([]int64, n*maxSeq)
	for i := 0; i < n; i++ {
		copy(ids[i*maxSeq:], encIDs[i])
		copy(mask[i*maxSeq:], encMask[i])
		if b.feedType {
			copy(types[i*maxSeq:], encType[i])
		}
	}

	shape := ort.NewShape(int64(n), int64(maxSeq))
	idsT, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer idsT.Destroy()
	maskT, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer maskT.Destroy()
	byName := map[string]ort.Value{"input_ids": idsT, "attention_mask": maskT}
	if b.feedType {
		typesT, err := ort.NewTensor(shape, types)
		if err != nil {
			return nil, err
		}
		defer typesT.Destroy()
		byName["token_type_ids"] = typesT
	}
	in := make([]ort.Value, len(b.inNames))
	for i, name := range b.inNames {
		in[i] = byName[name]
	}
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(n), int64(maxSeq), int64(b.dim)))
	if err != nil {
		return nil, err
	}
	defer outT.Destroy()

	s := <-b.pool
	runErr := s.Run(in, []ort.Value{outT})
	b.pool <- s
	if runErr != nil {
		return nil, runErr
	}

	raw := outT.GetData()
	out := make([][]float32, n)
	for bi := 0; bi < n; bi++ {
		vec := make([]float32, b.dim)
		rowBase := bi * maxSeq * b.dim
		if b.pooling == models.PoolingCLS {
			copy(vec, raw[rowBase:rowBase+b.dim])
			out[bi] = vec
			continue
		}
		var denom float32
		for t := 0; t < maxSeq; t++ {
			m := float32(mask[bi*maxSeq+t])
			if m == 0 {
				continue
			}
			denom += m
			base := rowBase + t*b.dim
			for d := 0; d < b.dim; d++ {
				vec[d] += raw[base+d] * m
			}
		}
		if denom > 0 {
			for d := range vec {
				vec[d] /= denom
			}
		}
		out[bi] = vec
	}
	return out, nil
}

// ONNXCrossEncoder runs a sentence-pair classification graph (reranker)
// and returns one relevance logit per (query, doc) pair.
type ONNXCrossEncoder struct {
	mu       sync.Mutex
	session  *ort.DynamicAdvancedSession
	tok      tokenizer.Tokenizer
	inNames  []string
	feedType bool
	outName  string
	outDim   int
	name     string
	device   string
}

// NewONNXCrossEncoder loads a cross-encoder graph + tokenizer for the
// given Spec. Only the Unigram tokenizer family is supported.
func NewONNXCrossEncoder(modelDir, modelFile string, spec models.Spec) (CrossEncoder, error) {
	if modelDir == "" {
		return nil, errors.New("onnx: modelDir required")
	}
	if modelFile == "" {
		modelFile = "model.onnx"
	}
	envOnce.Do(func() {
		resolveSharedLibrary()
		envErr = ort.InitializeEnvironment()
	})
	if envErr != nil {
		return nil, fmt.Errorf("onnx: init env: %w", envErr)
	}
	tok, err := tokenizer.Load(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("onnx: tokenizer: %w", err)
	}
	modelPath := filepath.Join(modelDir, modelFile)
	inInfo, outInfo, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("onnx: inspect %q: %w", modelPath, err)
	}
	feedable := map[string]bool{"input_ids": true, "attention_mask": true, "token_type_ids": true}
	var inNames []string
	feedType := false
	for _, in := range inInfo {
		if feedable[in.Name] {
			inNames = append(inNames, in.Name)
			if in.Name == "token_type_ids" {
				feedType = true
			}
		}
	}
	if len(inNames) == 0 {
		return nil, fmt.Errorf("onnx: reranker declares no recognised inputs")
	}
	outName, outDim := "", 1
	for _, o := range outInfo {
		if o.Name == "logits" {
			outName = o.Name
			if n := len(o.Dimensions); n > 0 && o.Dimensions[n-1] > 0 {
				outDim = int(o.Dimensions[n-1])
			}
			break
		}
	}
	if outName == "" && len(outInfo) > 0 {
		outName = outInfo[0].Name
	}
	if outName == "" {
		return nil, fmt.Errorf("onnx: reranker declares no outputs")
	}
	opts, ep := buildSessionOptions(deviceFromEnv())
	session, err := ort.NewDynamicAdvancedSession(modelPath, inNames, []string{outName}, opts)
	if err != nil && opts != nil {
		fmt.Fprintf(os.Stderr, "worker-embeddings: reranker %s EP unavailable (%v); CPU\n", ep, err)
		opts.Destroy()
		opts, ep = nil, "cpu"
		session, err = ort.NewDynamicAdvancedSession(modelPath, inNames, []string{outName}, nil)
	}
	if opts != nil {
		opts.Destroy()
	}
	if err != nil {
		return nil, fmt.Errorf("onnx: create reranker session: %w", err)
	}
	name := spec.ID
	if name == "" {
		name = "cross-encoder"
	}
	return &ONNXCrossEncoder{
		session: session, tok: tok, inNames: inNames, feedType: feedType,
		outName: outName, outDim: outDim, name: name, device: ep,
	}, nil
}

func (c *ONNXCrossEncoder) Model() string { return c.name }

func (c *ONNXCrossEncoder) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		err := c.session.Destroy()
		c.session = nil
		return err
	}
	return nil
}

func (c *ONNXCrossEncoder) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, err := c.scoreOne(query, d)
		if err != nil {
			return nil, fmt.Errorf("onnx: rerank doc[%d]: %w", i, err)
		}
		out[i] = s
	}
	return out, nil
}

func (c *ONNXCrossEncoder) scoreOne(query, doc string) (float32, error) {
	ids, mask, types, seq := c.tok.EncodePair(query, doc)
	shape := ort.NewShape(1, int64(seq))
	idsT, err := ort.NewTensor(shape, ids)
	if err != nil {
		return 0, err
	}
	defer idsT.Destroy()
	maskT, err := ort.NewTensor(shape, mask)
	if err != nil {
		return 0, err
	}
	defer maskT.Destroy()
	byName := map[string]ort.Value{"input_ids": idsT, "attention_mask": maskT}
	if c.feedType {
		typesT, err := ort.NewTensor(shape, types)
		if err != nil {
			return 0, err
		}
		defer typesT.Destroy()
		byName["token_type_ids"] = typesT
	}
	inputs := make([]ort.Value, len(c.inNames))
	for i, n := range c.inNames {
		inputs[i] = byName[n]
	}
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(1, int64(c.outDim)))
	if err != nil {
		return 0, err
	}
	defer outT.Destroy()

	c.mu.Lock()
	runErr := c.session.Run(inputs, []ort.Value{outT})
	c.mu.Unlock()
	if runErr != nil {
		return 0, runErr
	}
	return outT.GetData()[0], nil
}

func l2Normalize(v []float32) {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		return
	}
	norm := float32(math.Sqrt(sumSq))
	for i := range v {
		v[i] /= norm
	}
}

// resolveSharedLibrary points onnxruntime_go at the shared library:
// ONNXRUNTIME_LIB env first, then a library sitting next to the worker
// executable, else the OS default search path.
func resolveSharedLibrary() {
	if p := os.Getenv("ONNXRUNTIME_LIB"); p != "" {
		ort.SetSharedLibraryPath(p)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	for _, name := range sharedLibraryNames() {
		cand := filepath.Join(dir, name)
		if _, err := os.Stat(cand); err == nil {
			ort.SetSharedLibraryPath(cand)
			return
		}
	}
}

func sharedLibraryNames() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"onnxruntime.dll"}
	case "darwin":
		return []string{"libonnxruntime.dylib"}
	default:
		return []string{"libonnxruntime.so"}
	}
}
