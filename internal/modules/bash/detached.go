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

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func detachedArgs(kind, command string) []string {
	if kind == "powershell" {
		enc := base64.StdEncoding.EncodeToString(utf16leBytes(command))
		return []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", enc}
	}
	return append(shellArgs(kind), "-c", command)
}

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
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	} else if r := tool.StdinPipeFromContext(ctx); r != nil {
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
	}
	if sink := tool.LiveSinkFromContext(ctx); sink != nil {
		cmd.Stdout = io.MultiWriter(out, sink)
		cmd.Stderr = io.MultiWriter(errb, sink)
	}
	setSysProc(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
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
				switch context.Cause(cctx) {
				case errSignalTERM:
					res.ExitCode = 143
				default:
					res.ExitCode = 130
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
