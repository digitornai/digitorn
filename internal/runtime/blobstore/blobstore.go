package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

var ErrNotFound = errors.New("blobstore: not found")

type Store struct {
	root string

	mu sync.Mutex
}

func New(root string) *Store {
	return &Store{root: root}
}

func (s *Store) Put(ctx context.Context, mime string, r io.Reader) (sessionstore.BlobRef, error) {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: mkdir root: %w", err)
	}
	tmp, err := os.CreateTemp(s.root, "putting-*.tmp")
	if err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	kept := false
	defer func() {
		if !kept {
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	n, err := copyWithCtx(ctx, mw, r)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: copy: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	finalDir := s.shardDir(hash)
	finalPath := filepath.Join(finalDir, hash)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(finalPath); err == nil {
		return sessionstore.BlobRef{Hash: hash, Mime: mime, Size: n}, nil
	}
	if err := os.MkdirAll(finalDir, 0o700); err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: mkdir shard: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: rename: %w", err)
	}
	kept = true
	return sessionstore.BlobRef{Hash: hash, Mime: mime, Size: n}, nil
}

func (s *Store) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	if !isValidHash(hash) {
		return nil, fmt.Errorf("blobstore: invalid hash %q", hash)
	}
	f, err := os.Open(filepath.Join(s.shardDir(hash), hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("blobstore: open: %w", err)
	}
	return f, nil
}

func (s *Store) LoadBlob(ctx context.Context, hash string) ([]byte, error) {
	rc, err := s.Get(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (s *Store) Stat(ctx context.Context, hash string) (os.FileInfo, error) {
	if !isValidHash(hash) {
		return nil, fmt.Errorf("blobstore: invalid hash %q", hash)
	}
	info, err := os.Stat(filepath.Join(s.shardDir(hash), hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("blobstore: stat: %w", err)
	}
	return info, nil
}

func (s *Store) Exists(hash string) bool {
	if !isValidHash(hash) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.shardDir(hash), hash))
	return err == nil
}

func (s *Store) Delete(ctx context.Context, hash string) error {
	if !isValidHash(hash) {
		return fmt.Errorf("blobstore: invalid hash %q", hash)
	}
	err := os.Remove(filepath.Join(s.shardDir(hash), hash))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blobstore: delete: %w", err)
	}
	return nil
}

func (s *Store) shardDir(hash string) string {
	if len(hash) < 4 {
		return s.root
	}
	return filepath.Join(s.root, hash[:2], hash[2:4])
}

func isValidHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func copyWithCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	const chunk = 32 * 1024
	buf := make([]byte, chunk)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w != n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
