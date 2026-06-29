package bash

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

func detachedArgs(kind, command string) []string {
	if kind == "powershell" {
		// Use -EncodedCommand to avoid quoting issues with -Command:
		// Windows command-line parsing strips unescaped double quotes from
		// -Command arguments, corrupting commands that contain embedded
		// quotes (e.g. cmd /c "exit 0"). Base64-encoded command is immune
		// to quoting problems, but MUST be UTF-16LE (PowerShell's native
		// encoding) — plain UTF-8 base64 produces garbage characters that
		// cause parse errors like "Unexpected token '}'".
		enc := base64.StdEncoding.EncodeToString(utf16leBytes(command))
		return []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", enc}
	}
	return append(shellArgs(kind), "-c", command)
}

// utf16leBytes encodes a UTF-8 string as a UTF-16LE byte slice, which is
// what PowerShell's -EncodedCommand expects. Without this, characters are
// decoded as garbage (UTF-8 bytes misinterpreted as UTF-16LE code units),
// leading to spurious "Unexpected token" errors on curly braces and quotes.
func utf16leBytes(s string) []byte {
	runes := []rune(s)
	u16 := utf16.Encode(runes)
	b := make([]byte, len(u16)*2)
	for i, r := range u16 {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8)
	}
	return b
}

// runDetached executes a command as an independent one-shot process — used both
// for background_run dispatches (so a long task never blocks the session's
// shared shell) and for interactive `input` (the command's stdin is fed from
// input so it can answer prompts). stdout/stderr are drained concurrently into
// bounded buffers (no pipe deadlock), and on timeout OR caller cancellation the
// whole process group is reaped (Cmd.Cancel), so nothing is orphaned.
func runDetached(ctx context.Context, kind, path, command, dir string, env []string, maxOut int, input string, timeout time.Duration, promptWait time.Duration) (cmdResult, error) {
	var cctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		cctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	cmd := exec.CommandContext(cctx, path, detachedArgs(kind, command)...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out := newBoundedBuf(maxOut)
	errb := newBoundedBuf(maxOut)
	cmd.Stdout = out
	cmd.Stderr = errb
	if r := tool.StdinPipeFromContext(ctx); r != nil {
		if subStdin, pipeErr := cmd.StdinPipe(); pipeErr == nil {
			go func() {
				defer subStdin.Close()
				buf := make([]byte, 4096)
				for {
					n, rerr := r.Read(buf)
					if n > 0 {
						if _, werr := subStdin.Write(buf[:n]); werr != nil {
							break
						}
					}
					if rerr != nil {
						break
					}
				}
			}()
			go func() {
				<-cctx.Done()
				if rc, ok := r.(io.Closer); ok {
					_ = rc.Close()
				}
			}()
		}
	} else if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	// Live tail : if the caller (the BackgroundManager) attached a live sink,
	// tee the running command's stdout+stderr into it so a status check can show
	// the output WHILE the task is still running — not only when it finishes.
	if sink := tool.LiveSinkFromContext(ctx); sink != nil {
		cmd.Stdout = io.MultiWriter(out, sink)
		cmd.Stderr = io.MultiWriter(errb, sink)
	}
	setSysProc(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Check whether a graceful signal was requested via context cause
			// (set by the background manager's Signal() method). Falls back to
			// SIGKILL for plain cancellation or timeout.
			switch context.Cause(cctx) {
			case errSignalINT:
				signalProcessTree(cmd.Process.Pid, syscallSIGINT)
			case errSignalTERM:
				signalProcessTree(cmd.Process.Pid, syscallSIGTERM)
			default:
				killProcessTree(cmd.Process.Pid)
			}
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		return cmdResult{}, err
	}

	// Prompt detection: when no input is provided and promptWait > 0, monitor
	// stdout/stderr for a blocking interactive prompt. Two triggers:
	//   (a) a prompt-looking TRAILING line that then goes quiet for the grace
	//       window (a sudo/ssh password, a y/n confirmation) — catches the case
	//       a prompt IS printed (so the old silence-only check missed it), and
	//   (b) total silence for promptWait (a program reading stdin with no prompt).
	// On either, kill the tree and report waiting-for-input so the turn can't hang.
	var waitingForInput int32
	if input == "" && promptWait > 0 && cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			lastLen := 0
			lastGrowth := time.Now()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					cur := out.Len() + errb.Len()
					if cur != lastLen {
						lastLen = cur
						lastGrowth = time.Now()
					}
					idle := time.Since(lastGrowth)
					blockingPrompt := cur > 0 && idle >= promptGraceWindow &&
						(looksLikePrompt(out.String()) || looksLikePrompt(errb.String()))
					totalSilence := cur == 0 && idle >= promptWait
					if blockingPrompt || totalSilence {
						atomic.StoreInt32(&waitingForInput, 1)
						killProcessTree(cmd.Process.Pid)
						return
					}
				}
			}
		}()
		defer close(done)
	}

	waitErr := cmd.Wait()

	exit := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	res := cmdResult{Stdout: trimTrailing(out.String()), Stderr: trimTrailing(errb.String()), ExitCode: exit, Cwd: dir}
	if atomic.LoadInt32(&waitingForInput) == 1 {
		res.WaitingForInput = true
		return res, errWaitingForInput
	}
	if cctx.Err() != nil {
		if ctx.Err() != nil {
			res.Cancelled = true
			if res.ExitCode <= 0 {
				// Set the conventional exit code for the signal that was used.
				// bash exits with 128+N when killed by signal N (if no trap fires).
				// Go's ExitCode() returns -1 when a process is killed by signal,
				// so we fill in the right code here.
				switch context.Cause(cctx) {
				case errSignalTERM:
					res.ExitCode = 143 // 128 + SIGTERM(15)
				default:
					res.ExitCode = 130 // 128 + SIGINT(2) — default cancel
				}
			}
			return res, errCancelled
		}
		res.TimedOut = true
		if res.ExitCode <= 0 {
			res.ExitCode = 124
		}
		return res, errTimeout
	}
	return res, nil
}
