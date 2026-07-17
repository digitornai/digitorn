package bash

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/modules/bash/goshell"
)

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
		if res.Stderr == "" {
			res.Stderr = err.Error()
		}
		if res.ExitCode == 0 {
			res.ExitCode = 2
		}
	}
	return res
}
