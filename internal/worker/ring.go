package worker

import (
	"strings"
	"sync"
)

// ringBuffer keeps the last N lines for crash diagnostics.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
	pos   int
	full  bool
}

func newRingBuffer(cap int) *ringBuffer {
	if cap <= 0 {
		cap = 64
	}
	return &ringBuffer{lines: make([]string, cap), cap: cap}
}

func (r *ringBuffer) append(line string) {
	r.mu.Lock()
	r.lines[r.pos] = line
	r.pos = (r.pos + 1) % r.cap
	if r.pos == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

func (r *ringBuffer) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return strings.Join(r.lines[:r.pos], "\n")
	}
	var b strings.Builder
	b.Grow(r.cap * 64)
	for i := 0; i < r.cap; i++ {
		idx := (r.pos + i) % r.cap
		if r.lines[idx] == "" {
			continue
		}
		b.WriteString(r.lines[idx])
		b.WriteByte('\n')
	}
	return b.String()
}
