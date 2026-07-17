package indexer

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
)

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

type FileCursor struct {
	dir string
	mu  sync.Mutex
}

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
	return os.Rename(tmp, c.file(key))
}
