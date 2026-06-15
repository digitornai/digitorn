package background

import (
	"strings"
	"sync"
)

// maxLiveLog bounds the live tail kept per running task. The full output still
// arrives in the final result; this is just the rolling window an in-flight
// status check returns. Tuned high enough that a verbose dev server (vite,
// next, npm install) keeps several minutes of build output visible to the
// agent without blowing the prompt budget when sliced by tail_lines.
const maxLiveLog = 64 << 10 // 64 KB

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

// tailLines returns the most recent n lines of the buffered output, exactly
// what an agent watching a build / dev-server / install wants — last 50 or 100
// lines, formatted as a single newline-joined string so it slots into the
// existing `log` JSON field without changing the wire shape. n<=0 means "all
// bytes in the current window".
func (l *liveLog) tailLines(n int) string {
	full := l.tail()
	if n <= 0 || full == "" {
		return full
	}
	full = strings.TrimRight(full, "\n")
	lines := strings.Split(full, "\n")
	if len(lines) <= n {
		return full
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
