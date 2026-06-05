package background

import "sync"

// maxLiveLog bounds the live tail kept per running task. A status check shows
// the most recent output; the full output still arrives in the final result.
const maxLiveLog = 16 << 10 // 16 KB

// liveLog is a thread-safe, byte-bounded sink a running task streams its output
// into (via tool.WithLiveSink → the bash detached runner's io.MultiWriter), so a
// concurrent Status read can return a live tail. Keeps only the most recent
// maxLiveLog bytes; the backing array is trimmed lazily (at 2× cap) so a chatty
// task can't grow memory without bound.
type liveLog struct {
	mu  sync.Mutex
	buf []byte
}

func (l *liveLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > 2*maxLiveLog {
		l.buf = append([]byte(nil), l.buf[len(l.buf)-maxLiveLog:]...)
	}
	return len(p), nil
}

// tail returns the most recent ≤ maxLiveLog bytes.
func (l *liveLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > maxLiveLog {
		return string(l.buf[len(l.buf)-maxLiveLog:])
	}
	return string(l.buf)
}
