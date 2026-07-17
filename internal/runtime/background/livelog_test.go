package background

import (
	"strings"
	"testing"
)

func TestLiveLog_TailLines_ReturnsLastN(t *testing.T) {
	var l liveLog
	for i := 1; i <= 200; i++ {
		_, _ = l.Write([]byte("line " + itoa(i) + "\n"))
	}
	got := l.tailLines(50)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("tailLines(50): got %d lines, want 50", len(lines))
	}
	if lines[0] != "line 151" || lines[49] != "line 200" {
		t.Errorf("tail = [%q ... %q]", lines[0], lines[49])
	}
}

func TestLiveLog_TailLines_ZeroReturnsFullWindow(t *testing.T) {
	var l liveLog
	for i := 1; i <= 5; i++ {
		_, _ = l.Write([]byte("x" + itoa(i) + "\n"))
	}
	got := l.tailLines(0)
	if !strings.Contains(got, "x1") || !strings.Contains(got, "x5") {
		t.Errorf("tailLines(0) should be the full window, got %q", got)
	}
}

func TestLiveLog_TailLines_NLargerThanContent(t *testing.T) {
	var l liveLog
	_, _ = l.Write([]byte("only-one\n"))
	got := l.tailLines(50)
	if strings.TrimRight(got, "\n") != "only-one" {
		t.Errorf("tailLines(50) on 1-line buf = %q, want \"only-one\"", got)
	}
}

func TestLiveLog_TailLines_BoundedBy64KB(t *testing.T) {
	var l liveLog
	long := strings.Repeat("x", 200)
	for range 500 {
		_, _ = l.Write([]byte(long + "\n"))
	}
	full := l.tail()
	if len(full) > maxLiveLog {
		t.Fatalf("tail() exceeded maxLiveLog: %d > %d", len(full), maxLiveLog)
	}
	got := l.tailLines(10)
	if strings.Count(got, "\n")+1 > 11 {
		t.Errorf("tailLines(10): too many lines (%d)", strings.Count(got, "\n")+1)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}
