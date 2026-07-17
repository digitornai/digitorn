package goshell

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestCancel_KillsRunningChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	var out, errb bytes.Buffer
	start := time.Now()
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
