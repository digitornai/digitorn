package goshell

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestCancel_KillsRunningChild proves a long-running EXTERNAL child started by
// the shell dies promptly when the context is cancelled — the exact mechanism a
// background `sleep`/dev-server relies on to be cancellable. We cancel after
// 200ms a command that would otherwise block 30s; if the child were orphaned the
// Run call would hang until the test deadline.
func TestCancel_KillsRunningChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	var out, errb bytes.Buffer
	start := time.Now()
	// `sleep` resolves to busybox (no-bash host) or the host's sleep; either way
	// it's a real external process whose lifetime the shell must own.
	code, err := Run(ctx, `sleep 30`, t.TempDir(), nil, nil, &out, &errb)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("cancel did not stop the child: took %s (code=%d err=%v)", elapsed, code, err)
	}
	if ctx.Err() == nil {
		t.Fatalf("context was not cancelled; test is invalid")
	}
	t.Logf("cancelled child in %s (code=%d)", elapsed, code)
}

// TestCancel_StopsPureLoop proves the interpreter itself (no external process)
// observes cancellation between iterations — a busy bash loop is interruptible,
// not just external commands.
func TestCancel_StopsPureLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	var out, errb bytes.Buffer
	start := time.Now()
	code, _ := Run(ctx, `i=0; while true; do i=$((i+1)); done`, t.TempDir(), nil, nil, &out, &errb)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("cancel did not break the loop: took %s (code=%d)", elapsed, code)
	}
	t.Logf("broke pure loop in %s (code=%d)", elapsed, code)
}
