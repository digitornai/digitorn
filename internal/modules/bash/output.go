package bash

import (
	"fmt"
	"sync"
)

type boundedBuf struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int
}

func newBoundedBuf(max int) *boundedBuf {
	if max <= 0 {
		max = 1 << 20
	}
	return &boundedBuf{max: max}
}

func (b *boundedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.max - len(b.buf)
	if room <= 0 {
		b.dropped += len(p)
		return len(p), nil
	}
	if len(p) <= room {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:room]...)
	b.dropped += len(p) - room
	return len(p), nil
}

func (b *boundedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropped > 0 {
		return string(b.buf) + fmt.Sprintf("\n[truncated %d bytes]", b.dropped)
	}
	return string(b.buf)
}
