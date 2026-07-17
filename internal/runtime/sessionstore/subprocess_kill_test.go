package sessionstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSubprocessKill_KillMinusNine_RecoverWithoutLoss(t *testing.T) {
	if os.Getenv("DIGITORN_SESSIONSTORE_CHILD") == "1" {
		runChildWriter()
		return
	}

	tmp := t.TempDir()
	sid := "sess-killed"
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	cmd := exec.Command(exe,
		"-test.run", "TestSubprocessKill_KillMinusNine_RecoverWithoutLoss",
		"-test.v", "-test.timeout=20s")
	cmd.Env = append(os.Environ(),
		"DIGITORN_SESSIONSTORE_CHILD=1",
		"DIGITORN_SESSIONSTORE_CHILD_DIR="+tmp,
		"DIGITORN_SESSIONSTORE_CHILD_SID="+sid,
		"DIGITORN_SESSIONSTORE_CHILD_COUNT=200",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start child: %v", err)
	}

	progressCh := make(chan int, 64)
	go func() {
		defer close(progressCh)
		buf := make([]byte, 4096)
		var acc []byte
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				for {
					nl := indexByteCopy(acc, '\n')
					if nl < 0 {
						break
					}
					line := string(acc[:nl])
					acc = acc[nl+1:]
					if v, ok := parseProgress(line); ok {
						progressCh <- v
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var lastReported int
	killed := false
waitLoop:
	for {
		select {
		case v, ok := <-progressCh:
			if !ok {
				break waitLoop
			}
			lastReported = v
			if v >= 50 && !killed {
				killed = true
				if err := cmd.Process.Kill(); err != nil {
					t.Errorf("Kill: %v", err)
				}
			}
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatal("child did not progress to 50 events in 10s")
		}
	}
	_ = cmd.Wait()

	if !killed {
		t.Fatalf("child exited before we could kill it ; last reported = %d", lastReported)
	}
	t.Logf("killed child after it reported %d appended events", lastReported)

	paths := NewPaths(tmp)
	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("Load after kill: %v", err)
	}
	t.Logf("loaded after kill : events_applied=%d last_seq=%d bad_lines=%d partial=%v",
		loaded.EventsApplied, loaded.State.LastSeq, loaded.BadEventLines, loaded.State.Partial)

	if loaded.EventsApplied < lastReported {
		t.Fatalf("AppendDurable broke its contract : reported=%d on_disk=%d (LOST %d events)",
			lastReported, loaded.EventsApplied, lastReported-loaded.EventsApplied)
	}
	if loaded.EventsApplied > 0 && loaded.State.LastSeq < uint64(loaded.EventsApplied) {
		t.Fatalf("last_seq %d < events_applied %d", loaded.State.LastSeq, loaded.EventsApplied)
	}
}

func runChildWriter() {
	dir := os.Getenv("DIGITORN_SESSIONSTORE_CHILD_DIR")
	sid := os.Getenv("DIGITORN_SESSIONSTORE_CHILD_SID")
	countStr := os.Getenv("DIGITORN_SESSIONSTORE_CHILD_COUNT")
	if dir == "" || sid == "" {
		fmt.Fprintln(os.Stderr, "child: missing env")
		os.Exit(2)
	}
	count, _ := strconv.Atoi(countStr)
	if count == 0 {
		count = 200
	}
	paths := NewPaths(dir)
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        1,
		QueueCapPerShard: 256,
		BatchMax:         8,
		FlushInterval:    1 * time.Millisecond,
		Fsync:            true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child flusher:", err)
		os.Exit(3)
	}
	if err := flusher.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "child start:", err)
		os.Exit(3)
	}
	bus, err := NewBus(BusConfig{Paths: paths, Flusher: flusher})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child bus:", err)
		os.Exit(3)
	}
	if err := bus.Start(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "child bus start:", err)
		os.Exit(3)
	}

	for i := 0; i < count; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := bus.AppendDurable(ctx, Event{
			Type: EventUserMessage, SessionID: sid,
			Message: &MessagePayload{Role: "user", Content: fmt.Sprintf("event-%d", i)},
		})
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "child append:", err)
			os.Exit(4)
		}
		fmt.Printf("appended %d\n", i+1)
	}
	os.Exit(0)
}

func parseProgress(line string) (int, bool) {
	const prefix = "appended "
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(line[len(prefix):]))
	if err != nil {
		return 0, false
	}
	return v, true
}

func indexByteCopy(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

var _ = runtime.GOOS
var _ = filepath.Join
