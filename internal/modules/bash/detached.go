package bash

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

func detachedArgs(kind, command string) []string {
	if kind == "powershell" {
		return []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	return append(shellArgs(kind), "-c", command)
}

// runDetached executes a command as an independent one-shot process — used both
// for background_run dispatches (so a long task never blocks the session's
// shared shell) and for interactive `input` (the command's stdin is fed from
// input so it can answer prompts). stdout/stderr are drained concurrently into
// bounded buffers (no pipe deadlock), and on timeout OR caller cancellation the
// whole process group is reaped (Cmd.Cancel), so nothing is orphaned.
func runDetached(ctx context.Context, kind, path, command, dir string, env []string, maxOut int, input string, timeout time.Duration) (cmdResult, error) {
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
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	out := newBoundedBuf(maxOut)
	errb := newBoundedBuf(maxOut)
	cmd.Stdout = out
	cmd.Stderr = errb
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
			killProcessTree(cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		return cmdResult{}, err
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
	if cctx.Err() != nil {
		if ctx.Err() != nil {
			res.Cancelled = true
			if res.ExitCode <= 0 {
				res.ExitCode = 130
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
