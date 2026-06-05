//go:build !onnx

package backend

import (
	"errors"
)

// NewONNX is the no-op stub used when the binary is built WITHOUT
// the `onnx` build tag. Returns an error so the worker falls back
// to the deterministic backend with a clear log message.
//
// This file is purely a placeholder so callers (worker main) can
// reference NewONNX unconditionally — the real implementation
// (onnx_real.go, behind the build tag) replaces it when the user
// builds with `-tags onnx`.
//
// Why a stub : the doc-default model uses ONNX runtime via CGO.
// CGO breaks WASM and complicates mobile cross-compile. By
// gating the dependency behind a tag we keep the default Go
// build pure-Go while still supporting the production path.
//
// To enable :
//
//	go build -tags onnx ./cmd/digitorn-worker-embeddings
//
// The stub binary ships with the deterministic fallback only.
func NewONNX(modelDir string) (Backend, error) {
	return nil, errors.New("backend: ONNX support not compiled in (build with -tags onnx)")
}

// NewONNXWithFile is the stub variant — same not-compiled-in error.
func NewONNXWithFile(modelDir, modelFile string) (Backend, error) {
	return nil, errors.New("backend: ONNX support not compiled in (build with -tags onnx)")
}
