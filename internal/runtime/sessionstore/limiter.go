package sessionstore

import (
	"bufio"
	"context"
	"os"
)

type Limiter struct {
	ch chan struct{}
}

func NewLimiter(maxConcurrent int) *Limiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Limiter{ch: make(chan struct{}, maxConcurrent)}
}

func (l *Limiter) Acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		l.ch <- struct{}{}
		return nil
	}
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) Release() {
	if l == nil {
		return
	}
	select {
	case <-l.ch:
	default:
	}
}

func (l *Limiter) InFlight() int {
	if l == nil {
		return 0
	}
	return len(l.ch)
}

func newBufWriter(f *os.File) *bufio.Writer {
	return bufio.NewWriterSize(f, 64<<10)
}
