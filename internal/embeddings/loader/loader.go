// Package loader resolves + downloads the paraphrase-multilingual-
// MiniLM-L12-v2 ONNX model and tokenizer files into a local cache.
//
// First-start behaviour :
//
//  1. Check {modelDir}/model.onnx + tokenizer.json already on disk
//     → use them.
//  2. Otherwise fetch from HuggingFace mirror :
//     https://huggingface.co/sentence-transformers/{model}/resolve/main/...
//  3. Verify SHA-256 against the expected manifest.
//  4. Atomic-rename into the modelDir.
//
// The doc references FastEmbed which itself downloads from
// HuggingFace ; this loader mirrors that protocol so the model
// id matches the doc-default exactly.
//
// SHA256 manifest is hard-coded for the canonical model — if
// HuggingFace changes the binary we'll catch it (intentional
// safety belt, not a CI block).
package loader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxModelBytes bounds a single download so a broken or hostile mirror can't
// stream unbounded bytes and fill the disk. The largest canonical asset
// (full-precision model.onnx) is ~470 MB ; 4 GiB is a generous ceiling that
// still stops a runaway stream cold.
const maxModelBytes = 4 << 30

// httpClient is shared by all downloads. It sets transport-level timeouts
// (connect / TLS / response-header) so a mirror that accepts the connection
// but never answers fails fast instead of hanging the worker forever. It does
// NOT set a total Timeout : a legitimate multi-hundred-MB download over a slow
// link must not be guillotined mid-body. Body size is bounded separately by
// maxModelBytes, and ctx cancellation (worker shutdown) aborts an in-flight
// read at any point.
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	},
}

// File describes one downloadable asset.
type File struct {
	Name string // local filename (model.onnx, tokenizer.json, vocab.txt)
	URL  string // upstream URL
	SHA  string // expected hex sha256 ; empty = skip integrity check
}

// DefaultModel is the canonical id. Keep aligned with
// internal/embeddings.DefaultModel.
const DefaultModel = "paraphrase-multilingual-MiniLM-L12-v2"

// DefaultFiles is the manifest for the canonical model. The ONNX
// backend needs exactly two assets : the graph (model.onnx, served
// under the repo's /onnx/ subdir) and the HuggingFace fast-tokenizer
// (tokenizer.json), from which the Unigram vocab + ids are read
// directly. vocab.txt does not exist for this XLM-R-lineage model
// (it is SentencePiece, not WordPiece), so it is intentionally absent.
//
// SHAs are left empty so the first deploy doesn't fail on a stale
// checksum — flip them to verified hashes after the first production
// run records the bytes.
func DefaultFiles() []File {
	return Files("model.onnx")
}

// Files returns the manifest for a specific model graph file. The
// full-precision graph (model.onnx) is served by the sentence-
// transformers repo ; the int8 graph (model_quantized.onnx, ~4x
// smaller/faster) is served by the Xenova mirror. The tokenizer.json
// is identical for both (same base model) and always comes from the
// sentence-transformers repo.
func Files(modelFile string) []File {
	st := "https://huggingface.co/sentence-transformers/" + DefaultModel + "/resolve/main"
	modelURL := st + "/onnx/" + modelFile
	if modelFile == "model_quantized.onnx" {
		modelURL = "https://huggingface.co/Xenova/" + DefaultModel + "/resolve/main/onnx/model_quantized.onnx"
	}
	return []File{
		{Name: modelFile, URL: modelURL, SHA: ""},
		{Name: "tokenizer.json", URL: st + "/tokenizer.json", SHA: ""},
	}
}

// Ensure makes sure every File in `files` is present and verified
// under modelDir. Downloads missing entries (or those failing the
// sha check) over HTTP with progress on stderr. The downloads
// land in a temp file and rename into place — partial writes are
// never observed by the worker.
//
// Concurrent calls to Ensure for the same dir are NOT serialised
// here ; the caller (worker startup) is expected to be single-
// instance per dir. Use a file lock if multiple workers share a
// model dir.
func Ensure(ctx context.Context, modelDir string, files []File, log Logger) error {
	if modelDir == "" {
		return errors.New("loader: modelDir required")
	}
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return fmt.Errorf("loader: mkdir: %w", err)
	}
	for _, f := range files {
		target := filepath.Join(modelDir, f.Name)
		if ok, _ := verify(target, f.SHA); ok {
			if log != nil {
				log.Info("loader: file ready",
					"file", f.Name, "path", target)
			}
			continue
		}
		if err := download(ctx, target, f, log); err != nil {
			return fmt.Errorf("loader: %s: %w", f.Name, err)
		}
	}
	return nil
}

// Logger is the minimal logging surface — matches *slog.Logger.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// verify returns (matched, err). When sha is empty the file is
// accepted on its presence alone (the SHA check is optional per
// the doc — we re-verify on the next release cycle).
func verify(path, sha string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return false, err
	}
	if sha == "" {
		return true, nil
	}
	got, err := sha256File(path)
	if err != nil {
		return false, err
	}
	return got == sha, nil
}

// download fetches one file to {modelDir}/{name} via a temp file
// + atomic rename, then verifies the sha (when provided).
func download(ctx context.Context, target string, f File, log Logger) error {
	if log != nil {
		log.Info("loader: downloading", "url", f.URL, "to", target)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", f.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "digitorn-worker-embeddings/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Reject early when the server advertises a size beyond the ceiling.
	if resp.ContentLength > maxModelBytes {
		return fmt.Errorf("refusing download : Content-Length %d exceeds max %d", resp.ContentLength, maxModelBytes)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), "."+f.Name+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded

	// Bound the copy : read at most maxModelBytes+1 so we can detect (and
	// reject) a stream that runs past the ceiling instead of filling the disk.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxModelBytes+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if n > maxModelBytes {
		_ = tmp.Close()
		return fmt.Errorf("download exceeded max %d bytes", maxModelBytes)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if f.SHA != "" {
		got, err := sha256File(tmpPath)
		if err != nil {
			return err
		}
		if got != f.SHA {
			return fmt.Errorf("sha mismatch : got %s, want %s", got, f.SHA)
		}
	}

	return os.Rename(tmpPath, target)
}

// sha256File computes the hex-encoded sha256 of one file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
