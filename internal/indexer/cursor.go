package indexer

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
)

// MemCursor is an in-process Cursor for tests + the first live proof. The
// durable gateway-KV cursor (survives worker restarts) implements the same
// interface and swaps in without touching the service. See DESIGN.md §10.
type MemCursor struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemCursor() *MemCursor { return &MemCursor{m: map[string][]byte{}} }

func (c *MemCursor) Load(key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key], nil
}

func (c *MemCursor) Save(key string, state []byte) error {
	c.mu.Lock()
	c.m[key] = state
	c.mu.Unlock()
	return nil
}

// FileCursor persists sync state to a directory, one file per source key
// (key hashed for a safe filename). Durable across worker restarts on the
// same host : on restart, Walk diffs resume from the saved hashes (no
// re-embed storm) and CDC from the saved LSN. (Kafka offsets + the Postgres
// replication slot are durable server-side regardless.)
type FileCursor struct {
	dir string
	mu  sync.Mutex
}

// NewFileCursor creates the directory and returns a file-backed cursor.
func NewFileCursor(dir string) (*FileCursor, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileCursor{dir: dir}, nil
}

func (c *FileCursor) file(key string) string {
	h := sha1.Sum([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:]))
}

func (c *FileCursor) Load(key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(c.file(key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

func (c *FileCursor) Save(key string, state []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tmp := c.file(key) + ".tmp"
	if err := os.WriteFile(tmp, state, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.file(key)) // atomic replace
}
