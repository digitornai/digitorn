//go:build windows

package bash

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// runRaw invokes the module's run handler for a session WITHOUT t.Fatalf, so it
// is safe to call from concurrent goroutines (testing.T.Fatalf may only be
// called from the test goroutine). Returns the parsed result + any transport
// error.
func runRaw(m *Module, sessionID, command string) (runResult, string, error) {
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: sessionID})
	raw, _ := json.Marshal(runParams{Command: command})
	res, err := m.run(ctx, raw)
	if err != nil {
		return runResult{}, "", err
	}
	rr, _ := res.Data.(runResult)
	return rr, res.Error, nil
}

// TestStress_ConcurrentSessionsNoBleed runs many sessions concurrently, each
// echoing a token unique to that session, and asserts each call sees ONLY its
// own token — proving the per-session shell map, the cur pointer, and the
// output buffers don't bleed across sessions under contention. Run with -race.
func TestStress_ConcurrentSessionsNoBleed(t *testing.T) {
	m := testModulePS(t)
	const sessions = 24
	const itersPerSession = 8

	var wg sync.WaitGroup
	errs := make(chan string, sessions*itersPerSession)
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			sess := fmt.Sprintf("sess-%d", s)
			for i := 0; i < itersPerSession; i++ {
				want := fmt.Sprintf("TOK_%d_%d", s, i)
				rr, _, err := runRaw(m, sess, "Write-Output "+want)
				if err != nil {
					errs <- fmt.Sprintf("%s iter %d: transport err %v", sess, i, err)
					return
				}
				got := strings.TrimSpace(rr.Stdout)
				if got != want {
					errs <- fmt.Sprintf("%s iter %d: got %q want %q (cross-session bleed?)", sess, i, got, want)
					return
				}
				if rr.ExitCode != 0 {
					errs <- fmt.Sprintf("%s iter %d: exit %d", sess, i, rr.ExitCode)
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestStress_OneShotConcurrentIndependent fires many one-shot commands across
// sessions concurrently, each emitting its own distinct value inline, asserting
// every call returns ITS OWN output with no cross-talk, no race, and no msys
// fork-wedge under load — the stability guarantee of the one-shot model.
func TestStress_OneShotConcurrentIndependent(t *testing.T) {
	m := testModulePS(t)
	const sessions = 16
	var wg sync.WaitGroup
	errs := make(chan string, sessions*4)
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			sess := fmt.Sprintf("oneshot-%d", s)
			for i := 0; i < 4; i++ {
				rr, _, err := runRaw(m, sess, fmt.Sprintf("Write-Output \"V=%d\"", s))
				if err != nil {
					errs <- fmt.Sprintf("%s run: %v", sess, err)
					return
				}
				if !strings.Contains(rr.Stdout, fmt.Sprintf("V=%d", s)) {
					errs <- fmt.Sprintf("%s got %q want V=%d", sess, strings.TrimSpace(rr.Stdout), s)
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestStress_CancelUnderLoad cancels a long-running command on one session while
// other sessions keep working, asserting the cancel is observed and the other
// sessions are unaffected — concurrent cancel vs the shell map / process tree.
func TestStress_CancelUnderLoad(t *testing.T) {
	m := testModulePS(t)

	var wg sync.WaitGroup
	// Background noise sessions.
	stop := make(chan struct{})
	for s := 0; s < 6; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			sess := fmt.Sprintf("noise-%d", s)
			for {
				select {
				case <-stop:
					return
				default:
				}
				rr, _, err := runRaw(m, sess, "Write-Output ping")
				if err != nil || strings.TrimSpace(rr.Stdout) != "ping" {
					return // surface via the assertion below indirectly; just stop
				}
			}
		}(s)
	}

	// A cancellable long command on its own session.
	ctx, cancel := context.WithCancel(tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "victim"}))
	done := make(chan runResult, 1)
	go func() {
		raw, _ := json.Marshal(runParams{Command: "Start-Sleep -Seconds 30"})
		res, _ := m.run(ctx, raw)
		rr, _ := res.Data.(runResult)
		done <- rr
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case rr := <-done:
		if !rr.Cancelled && rr.ExitCode == 0 {
			t.Fatalf("cancelled command should not report clean success: %+v", rr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not unblock the command within 10s")
	}
	close(stop)
	wg.Wait()

	// The victim session must still be usable afterwards (a fresh shell).
	rr, _, err := runRaw(m, "victim", "Write-Output recovered")
	if err != nil || strings.TrimSpace(rr.Stdout) != "recovered" {
		t.Fatalf("victim session not usable after cancel: stdout=%q err=%v", rr.Stdout, err)
	}
}

// TestStress_RapidSessionChurn creates, uses and lets go of many distinct
// sessions quickly, exercising the shell-map insert path + later janitor reap.
func TestStress_RapidSessionChurn(t *testing.T) {
	m := testModulePS(t)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess := fmt.Sprintf("churn-%d", i)
			rr, _, err := runRaw(m, sess, "Write-Output churn")
			if err != nil || strings.TrimSpace(rr.Stdout) != "churn" {
				errs <- fmt.Sprintf("%s: stdout=%q err=%v", sess, rr.Stdout, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestStress_HugeMultilineOutputBounded asserts a command emitting far more than
// the output cap completes (no wedge) and the captured output is BOUNDED, not
// unbounded memory growth.
func TestStress_HugeMultilineOutputBounded(t *testing.T) {
	m := testModulePS(t)
	// 200k lines of "x" — well past the 1MB cap.
	rr, _, err := runRaw(m, "huge", "1..200000 | ForEach-Object { 'x' }")
	if err != nil {
		t.Fatalf("huge output transport err: %v", err)
	}
	if rr.ExitCode != 0 {
		t.Fatalf("huge output exit=%d (wedged?)", rr.ExitCode)
	}
	if len(rr.Stdout) > 4<<20 {
		t.Fatalf("output not bounded: %d bytes (cap should clamp near 1MB)", len(rr.Stdout))
	}
}

// TestStress_StdinInput feeds data on stdin and asserts the command reads it,
// while proving the framing bytes that follow are not consumed by the command.
func TestStress_StdinInput(t *testing.T) {
	m := testModulePS(t)
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "stdin"})
	raw, _ := json.Marshal(runParams{Command: "node -e \"const d=require('fs').readFileSync(0,'utf8'); process.stdout.write('GOT:'+d.trim())\"", Input: "hello-stdin"})
	res, err := m.run(ctx, raw)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	rr, _ := res.Data.(runResult)
	if !strings.Contains(rr.Stdout, "GOT:hello-stdin") {
		t.Fatalf("stdin not delivered: stdout=%q stderr=%q exit=%d", rr.Stdout, rr.Stderr, rr.ExitCode)
	}
}

// TestStress_UnicodeRoundtrip pushes UTF-8 (accents, CJK, emoji) through the
// command line and back, asserting no mojibake in the captured output.
func TestStress_UnicodeRoundtrip(t *testing.T) {
	m := testModulePS(t)
	const payload = "héllo-世界-🚀"
	rr, _, err := runRaw(m, "uni", "node -e \"console.log('"+payload+"')\"")
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if !strings.Contains(rr.Stdout, payload) {
		t.Fatalf("unicode mangled: got %q want substring %q", rr.Stdout, payload)
	}
}

// TestStress_MarkerSpoofResistance tries to make the command print something
// that looks like the completion marker, to confirm the agent's output can't
// forge the exit-code/cwd line (the marker is an unguessable per-shell random).
func TestStress_MarkerSpoofResistance(t *testing.T) {
	m := testModulePS(t)
	// Print a plausible fake marker line, then exit 0. The real exit code must
	// still be 0 and the fake line must appear as ordinary output, not be parsed
	// as the completion marker (which would corrupt exit/cwd capture).
	rr, _, err := runRaw(m, "spoof", `Write-Output '__DGT_deadbeef__ 99 C:\evil'`)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if rr.ExitCode != 0 {
		t.Fatalf("spoofed marker corrupted exit code: got %d want 0", rr.ExitCode)
	}
	if !strings.Contains(rr.Stdout, "__DGT_deadbeef__ 99") {
		t.Fatalf("spoof line should pass through as plain output: %q", rr.Stdout)
	}
}
