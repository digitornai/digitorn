// Package blobstore is the daemon's content-addressed binary store. It
// backs multimedia messages (image / audio / video / file attachments)
// and tool results that carry non-text payloads.
//
// Properties guaranteed :
//
//   - Content-addressed : hash = sha256(bytes). Same bytes → same hash →
//     same on-disk file. Dedup is automatic (10 users uploading the
//     same image = 1 physical blob).
//   - Atomic writes : Put writes to a `.tmp` file then renames into
//     place. A crash mid-write never leaves a half-written blob with
//     a valid name.
//   - Streamed hashing : Put reads the body in 32KB chunks and hashes
//     on the fly. A 1GB upload is O(1) RAM.
//   - Sharded layout : `<root>/<hash[:2]>/<hash[2:4]>/<hash>` keeps any
//     single directory under ~65k entries even at billions of blobs.
//
// V1 is disk-only (no S3 / no interface) per the constraint that we
// keep the daemon self-contained. The package's surface is small
// enough that a refactor to a pluggable backend later is cheap.
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

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// ErrNotFound is returned by Get / Stat when the requested hash isn't
// in the store. Wrap with fmt.Errorf so callers can use errors.Is.
var ErrNotFound = errors.New("blobstore: not found")

// Store is the disk-backed content-addressed store. Thread-safe : Put
// and Get can be called concurrently from any number of goroutines.
// The directory mutex (mu) is only held during the rename in Put — so
// concurrent reads NEVER block, and concurrent writes for DIFFERENT
// hashes don't block each other either.
type Store struct {
	root string

	// mu serialises just the rename step in Put — keeps two concurrent
	// writers of the SAME hash from racing each other into the rename.
	// Reads are lock-free.
	mu sync.Mutex
}

// New constructs a store rooted at `root`. The directory is created on
// demand the first time a blob is written ; New itself does NOT touch
// the filesystem so it's safe to call from constructors that haven't
// yet decided whether the store will be used.
func New(root string) *Store {
	return &Store{root: root}
}

// Put writes the bytes from r into the store and returns the resulting
// BlobRef. Streams the body so memory usage is constant regardless of
// blob size.
//
// If a blob with the same hash already exists, Put discards the temp
// file (dedup) and returns the existing ref. Mime is the caller's hint
// and is recorded only in the returned BlobRef — the store itself is
// content-only.
func (s *Store) Put(ctx context.Context, mime string, r io.Reader) (sessionstore.BlobRef, error) {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: mkdir root: %w", err)
	}
	// Write to a single tmp file in root ; we don't know the final shard
	// until we've hashed every byte, so the tmp can't sit in the final
	// shard yet.
	tmp, err := os.CreateTemp(s.root, "putting-*.tmp")
	if err != nil {
		return sessionstore.BlobRef{}, fmt.Errorf("blobstore: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	// On any error past this point we must remove the tmp file ; deferred
	// remove + a "kept" flag we flip when the rename succeeds.
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
		// Already on disk — dedup. The tmp will be removed by the defer.
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

// Get opens the blob and returns a ReadCloser. Returns ErrNotFound if
// the hash isn't in the store. Caller MUST Close the reader.
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

// LoadBlob reads a stored blob fully into memory. It adapts Get to the
// engine/adapter's BlobLoader contract (LoadBlob(ctx, hash) ([]byte, error)),
// so the same *Store wires both the read (Get) and the LLM-adapter (LoadBlob)
// sides. Returns empty bytes (not an error) for an unknown hash so a missing
// blob degrades to "dropped part" rather than failing the whole turn.
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

// Stat returns the file info for a stored blob. Useful for size lookup
// without opening the file. ErrNotFound on unknown hash.
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

// Exists is a cheap presence check. Returns false on any error
// (including missing) — callers who care about the distinction should
// use Stat directly.
func (s *Store) Exists(hash string) bool {
	if !isValidHash(hash) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.shardDir(hash), hash))
	return err == nil
}

// Delete removes a blob from disk. Idempotent : returns nil if the blob
// doesn't exist. Compaction calls this for blobs whose last referencing
// event got purged.
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

// shardDir returns the directory where a blob with the given hash
// would live. Two levels of 2-hex-char shards keep any single dir
// well below FS limits even at billion-scale.
func (s *Store) shardDir(hash string) string {
	if len(hash) < 4 {
		return s.root
	}
	return filepath.Join(s.root, hash[:2], hash[2:4])
}

// isValidHash returns true when the string looks like a SHA-256 hex
// digest. Cheap guard against path-traversal / malformed callers.
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

// copyWithCtx is io.Copy with ctx cancellation. Important on long-
// running uploads ; the caller's context cancel must abort the copy
// promptly so we don't keep a tmp file open after a client disconnect.
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
