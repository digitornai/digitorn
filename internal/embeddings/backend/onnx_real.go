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
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/mbathepaul/digitorn/internal/embeddings/backend/tokenizer"
)

// envOnce guards the one-time onnxruntime environment initialisation.
// The environment is process-global in onnxruntime, so every backend
// instance shares it.
var (
	envOnce sync.Once
	envErr  error
)

// ONNXBackend holds one reusable inference session. The model is loaded
// once (not per call) ; Run is serialised by mu because an onnxruntime
// session is not safe for concurrent Run from multiple goroutines.
type ONNXBackend struct {
	mu       sync.Mutex
	session  *ort.DynamicAdvancedSession
	tok      *tokenizer.Unigram
	inNames  []string // model input order, restricted to those we feed
	feedType bool     // model declares token_type_ids
	outName  string
	dim      int
	device   string // execution provider actually in use (cpu/directml/cuda/coreml)
}

// NewONNX loads the doc-default full-precision graph (model.onnx).
func NewONNX(modelDir string) (Backend, error) {
	return NewONNXWithFile(modelDir, "model.onnx")
}

// NewONNXWithFile loads the named ONNX graph + tokenizer from modelDir
// and builds a persistent session. modelFile is "model.onnx" (default)
// or "model_quantized.onnx" (the int8 variant — ~4x smaller/faster,
// same 384-dim output and tokenizer).
func NewONNXWithFile(modelDir, modelFile string) (Backend, error) {
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

	tok, err := tokenizer.NewUnigram(filepath.Join(modelDir, "tokenizer.json"))
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
	if dim == 0 {
		dim = 384 // doc-default hidden size
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

	return &ONNXBackend{
		session:  session,
		tok:      tok,
		inNames:  inNames,
		feedType: feedType,
		outName:  outName,
		dim:      dim,
		device:   ep,
	}, nil
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

func (b *ONNXBackend) Model() string  { return "paraphrase-multilingual-MiniLM-L12-v2" }
func (b *ONNXBackend) Dimension() int { return b.dim }

// Device reports the execution provider in use (cpu/directml/cuda/coreml).
func (b *ONNXBackend) Device() string { return b.device }

func (b *ONNXBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		err := b.session.Destroy()
		b.session = nil
		return err
	}
	return nil
}

// Embed runs one forward pass per input, mean-pools the token hidden
// states (attention-mask weighted) and optionally L2-normalises.
func (b *ONNXBackend) Embed(ctx context.Context, inputs []string, l2norm bool) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, text := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vec, err := b.embedOne(text)
		if err != nil {
			return nil, fmt.Errorf("onnx: input[%d]: %w", i, err)
		}
		if l2norm {
			l2Normalize(vec)
		}
		out[i] = vec
	}
	return out, nil
}

func (b *ONNXBackend) embedOne(text string) ([]float32, error) {
	ids, mask, types, seq := b.tok.Encode(text)
	shape := ort.NewShape(1, int64(seq))

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

	byName := map[string]ort.Value{
		"input_ids":      idsT,
		"attention_mask": maskT,
	}
	if b.feedType {
		typesT, err := ort.NewTensor(shape, types)
		if err != nil {
			return nil, err
		}
		defer typesT.Destroy()
		byName["token_type_ids"] = typesT
	}
	inputs := make([]ort.Value, len(b.inNames))
	for i, name := range b.inNames {
		inputs[i] = byName[name]
	}

	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(1, int64(seq), int64(b.dim)))
	if err != nil {
		return nil, err
	}
	defer outT.Destroy()

	b.mu.Lock()
	runErr := b.session.Run(inputs, []ort.Value{outT})
	b.mu.Unlock()
	if runErr != nil {
		return nil, runErr
	}

	// Mean-pool over the sequence dimension, weighted by attention mask.
	raw := outT.GetData()
	vec := make([]float32, b.dim)
	var denom float32
	for t := 0; t < seq; t++ {
		m := float32(mask[t])
		denom += m
		base := t * b.dim
		for d := 0; d < b.dim; d++ {
			vec[d] += raw[base+d] * m
		}
	}
	if denom > 0 {
		for d := range vec {
			vec[d] /= denom
		}
	}
	return vec, nil
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
