package bash

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/modules/bash/goshell"
)

// runGoShell executes a command with the built-in pure-Go bash interpreter and
// returns the same cmdResult shape as the subprocess path, so result() and the
// rest of the module are oblivious to which backend ran. Output is capped, the
// timeout reaps via context, and `input` is fed on stdin.
func runGoShell(ctx context.Context, command, dir string, env []string, maxOut int, input string, timeout time.Duration) cmdResult {
	cctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var stdin io.Reader
	if input != "" {
		stdin = strings.NewReader(input)
	}
	// Same bounded buffer as the subprocess path: it caps memory AND appends a
	// `[truncated N bytes]` marker, so a very large output is never silently cut
	// — the agent always learns the truth was clipped. trimTrailing matches the
	// subprocess result shape (drop only the trailing newline, keep the marker).
	stdout := newBoundedBuf(maxOut)
	stderr := newBoundedBuf(maxOut)

	code, err := goshell.Run(cctx, command, dir, env, stdin, stdout, stderr)

	res := cmdResult{
		Stdout:   trimTrailing(stdout.String()),
		Stderr:   trimTrailing(stderr.String()),
		ExitCode: code,
		Cwd:      dir,
	}
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = -1
	case errors.Is(ctx.Err(), context.Canceled):
		res.Cancelled = true
		res.ExitCode = -1
	case err != nil:
		// Parse / setup error (not a normal non-zero exit): surface it.
		if res.Stderr == "" {
			res.Stderr = err.Error()
		}
		if res.ExitCode == 0 {
			res.ExitCode = 2
		}
	}
	return res
}
