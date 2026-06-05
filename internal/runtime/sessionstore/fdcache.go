package sessionstore

import (
	"container/list"
	"fmt"
	"os"
	"sync"
)

type fdCache struct {
	mu      sync.Mutex
	max     int
	entries map[string]*fdEntry
	order   *list.List
}

type fdEntry struct {
	path string
	f    *os.File
	elem *list.Element
}

func newFDCache(max int) *fdCache {
	if max <= 0 {
		max = 1024
	}
	return &fdCache{
		max:     max,
		entries: make(map[string]*fdEntry, max),
		order:   list.New(),
	}
}

func (c *fdCache) Get(path string) (*os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[path]; ok {
		c.order.MoveToFront(e.elem)
		return e.f, nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fd open %s: %w", path, err)
	}

	for len(c.entries) >= c.max {
		back := c.order.Back()
		if back == nil {
			break
		}
		evictPath := back.Value.(string)
		if e := c.entries[evictPath]; e != nil {
			_ = e.f.Close()
			delete(c.entries, evictPath)
		}
		c.order.Remove(back)
	}

	elem := c.order.PushFront(path)
	c.entries[path] = &fdEntry{path: path, f: f, elem: elem}
	return f, nil
}

func (c *fdCache) Drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[path]; ok {
		_ = e.f.Close()
		c.order.Remove(e.elem)
		delete(c.entries, path)
	}
}

func (c *fdCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		_ = e.f.Close()
	}
	c.entries = map[string]*fdEntry{}
	c.order.Init()
}

func (c *fdCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
