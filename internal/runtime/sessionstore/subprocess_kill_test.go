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

// TestSubprocessKill_KillMinusNine_RecoverWithoutLoss spawns the test
// binary in a child process role ("childWriter"), waits until the
// child has reported its events as DURABLE (it explicitly flushes
// after each append before printing the progress line), then SIGKILLs
// it. The parent loads the session from disk and verifies that every
// durable-reported event survived the kill.
//
// What this proves :
//   - Flush(ctx) is a real durability barrier (not just a hint).
//   - Load() after kill recovers everything below the kill point
//     without duplicates and without phantom events.
//   - Real OS semantics (partial fsync, page cache flush at process
//     death, locked file descriptors) are honoured.
//
// What this does NOT prove :
//   - That AppendBlocking alone gives durability. It does not — see
//     H3-G1 for the design decision to add AppendDurable. The child
//     here intentionally calls flusher.Flush() after every append.
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

	// Spawn the child with a custom env flag so it runs `runChildWriter`
	// instead of executing the test suite.
	cmd := exec.Command(exe,
		"-test.run", "TestSubprocessKill_KillMinusNine_RecoverWithoutLoss",
		"-test.v", "-test.timeout=20s")
	cmd.Env = append(os.Environ(),
		"DIGITORN_SESSIONSTORE_CHILD=1",
		"DIGITORN_SESSIONSTORE_CHILD_DIR="+tmp,
		"DIGITORN_SESSIONSTORE_CHILD_SID="+sid,
		"DIGITORN_SESSIONSTORE_CHILD_COUNT=200",
	)

	// Capture stdout so we can read the child's progress markers.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start child: %v", err)
	}

	// Read child's progress line by line ; once it reports >= 50
	// appends, kill it.
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
	_ = cmd.Wait() // exit will be a kill signal — non-zero, expected.

	if !killed {
		t.Fatalf("child exited before we could kill it ; last reported = %d", lastReported)
	}
	t.Logf("killed child after it reported %d appended events", lastReported)

	// Now load the session from disk. The child wrote its progress
	// AFTER each successful AppendBlocking returned, so every reported
	// event MUST be on disk (modulo OS page cache that we expect the
	// flusher to have flushed via fsync).
	paths := NewPaths(tmp)
	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("Load after kill: %v", err)
	}
	t.Logf("loaded after kill : events_applied=%d last_seq=%d bad_lines=%d partial=%v",
		loaded.EventsApplied, loaded.State.LastSeq, loaded.BadEventLines, loaded.State.Partial)

	// We tolerate the in-flight last event being lost (page cache).
	// We do NOT tolerate losing events the child reported as committed
	// more than 1 ago — those MUST be on disk because the flusher
	// fsyncs every batch.
	// AppendDurable contract : EVERY reported event must be on disk.
	// No "minus 1" tolerance — the durability promise is absolute.
	if loaded.EventsApplied < lastReported {
		t.Fatalf("AppendDurable broke its contract : reported=%d on_disk=%d (LOST %d events)",
			lastReported, loaded.EventsApplied, lastReported-loaded.EventsApplied)
	}
	// Sequences must be monotonic.
	if loaded.EventsApplied > 0 && loaded.State.LastSeq < uint64(loaded.EventsApplied) {
		t.Fatalf("last_seq %d < events_applied %d", loaded.State.LastSeq, loaded.EventsApplied)
	}
}

// runChildWriter is what the spawned child does : open a Bus on the
// dir from env, append events as fast as possible, print "appended N"
// on stdout after each success, then loop forever (parent will kill).
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
		// AppendDurable returns ONLY after fsync — the API itself is
		// the durability contract. No manual flush needed.
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
	// Child finished without being killed — exit cleanly so parent
	// knows it should have killed sooner.
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

// Silence "unused" complaints if runtime is imported but build tags
// skip the test on some platform.
var _ = runtime.GOOS
var _ = filepath.Join
