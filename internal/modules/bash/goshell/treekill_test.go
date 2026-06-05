package goshell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCancel_ReapsGrandchild is the decisive proof that cancel reaps the WHOLE
// tree, not just the direct child — the npm→node→workers case.
//
//	goshell ──exec──▶ sh (child, stays alive on `wait`)
//	                   └─ subshell (grandchild) ── loop: append heartbeat; sleep
//
// The heartbeat is written by the GRANDCHILD. If cancel killed only the direct
// child, the grandchild would be reparented and keep writing. We cancel mid-run,
// then watch the file: it must STOP growing. The grandchild loop is bounded (~10s
// self-terminating) so a regression can never leave a permanent process on the
// host running the test.
func TestCancel_ReapsGrandchild(t *testing.T) {
	if busyboxOrShell(t) == "" {
		t.Skip("no sh available")
	}
	dir := t.TempDir()
	hb := filepath.Join(dir, "hb.txt")
	fwd := strings.ReplaceAll(hb, "\\", "/")

	// Grandchild = a backgrounded subshell that heartbeats; child = sh that waits
	// on it so the whole tree stays alive until cancel. Loop is capped at 50 ticks.
	script := fmt.Sprintf(
		`sh -c '(i=0; while [ $i -lt 50 ]; do echo x >> "%s"; sleep 0.2; i=$((i+1)); done) & wait'`, fwd)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(700 * time.Millisecond) // let a few heartbeats land
		cancel()
	}()

	var out, errb bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, script, dir, os.Environ(), nil, &out, &errb)
		close(done)
	}()

	<-done // Run returned (tree killed). Snapshot the heartbeat size now.
	sizeAtCancel := fileSize(hb)
	if sizeAtCancel == 0 {
		t.Fatalf("grandchild never wrote a heartbeat — test setup invalid (stderr=%q)", errb.String())
	}

	// If the grandchild survived, it keeps writing every 200ms; 1.5s ≈ 7 more
	// ticks. A reaped tree writes nothing further (allow one in-flight write).
	time.Sleep(1500 * time.Millisecond)
	grown := fileSize(hb) - sizeAtCancel
	if grown > 2 {
		t.Fatalf("grandchild ORPHANED: heartbeat grew %d bytes after cancel (tree not reaped)", grown)
	}
	t.Logf("tree reaped: %d bytes at cancel, +%d after 1.5s", sizeAtCancel, grown)
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// busybox provides sh on a no-bash host; otherwise the host's sh is on PATH.
func busyboxOrShell(t *testing.T) string {
	t.Helper()
	var out, errb bytes.Buffer
	code, _ := Run(context.Background(), `sh -c 'echo ok'`, t.TempDir(), os.Environ(), nil, &out, &errb)
	if code == 0 && strings.TrimSpace(out.String()) == "ok" {
		return "ok"
	}
	return ""
}
