package bash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runMvdanSh executes a shell command through the mvdan/sh pure-Go interpreter.
// It returns the same cmdResult shape as runGoShell and runDetached so the rest
// of the module (result(), enrich(), …) is completely oblivious to the backend.
//
// mvdan/sh advantages over goshell:
//   - Full POSIX sh + bash extensions: arrays, [[ ]], $(…), here-docs, pipelines
//   - Real OS process spawning for external binaries (node, npm, git, python…)
//   - Identical behaviour on Windows, Linux, macOS — no MSYS, no path rewriting
//   - Proper /dev/null and nul handling cross-platform
//   - Timeout and cancellation via context — the whole pipeline is reaped

func envToSlice(env expand.Environ) []string {
	var out []string
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.IsSet() {
			out = append(out, name+"="+vr.String())
		}
		return true
	})
	return out
}

func runMvdanSh(ctx context.Context, command, dir string, env []string, maxOut int, input string, timeout time.Duration) cmdResult {
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

	f, parseErr := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if parseErr != nil {
		return cmdResult{
			Stdout:   "",
			Stderr:   fmt.Sprintf("shell parse error: %v", parseErr),
			ExitCode: 2,
			Cwd:      dir,
		}
	}

	runner, err := interp.New(
		interp.Dir(dir),
		interp.StdIO(stdin, stdout, stderr),
		interp.Env(expand.ListEnviron(env...)),
		// ExecHandlers: built-ins (cd, export, echo, test, [[ ]]…) are handled
		// in-process by mvdan/sh; external commands (node, npm, git, python, go,
		// docker, curl…) are spawned as real OS processes. This makes the
		// interpreter production-grade: every CLI the agent needs runs natively.
		interp.ExecHandlers(func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
			return func(ctx context.Context, args []string) error {
				path, lookErr := exec.LookPath(args[0])
				if lookErr != nil {
					// Try next handler (mvdan built-ins) before giving up.
					return next(ctx, args)
				}
				hc := interp.HandlerCtx(ctx)
				cmd := exec.CommandContext(ctx, path, args[1:]...)
				cmd.Dir = hc.Dir
				cmd.Env = envToSlice(hc.Env)
				cmd.Stdin = hc.Stdin
				cmd.Stdout = hc.Stdout
				cmd.Stderr = hc.Stderr
				if err := cmd.Run(); err != nil {
					// Preserve the exit code so $? and && / || work correctly.
					var exitErr *exec.ExitError
					if errors.As(err, &exitErr) {
						return interp.NewExitStatus(uint8(exitErr.ExitCode()))
					}
					return err
				}
				return nil
			}
		}),
		// OpenHandler: makes >, >>, 2>/dev/null, and file redirections work
		// correctly on every platform including Windows (where /dev/null is nul).
		interp.OpenHandler(func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
			if path == "/dev/null" || strings.EqualFold(path, "nul") {
				return mvdanDevNull{}, nil
			}
			return os.OpenFile(path, flag, perm)
		}),
	)
	if err != nil {
		return cmdResult{
			Stdout:   "",
			Stderr:   fmt.Sprintf("shell init error: %v", err),
			ExitCode: 2,
			Cwd:      dir,
		}
	}

	runErr := runner.Run(cctx, f)

	res := cmdResult{
		Stdout: trimTrailing(stdout.String()),
		Stderr: trimTrailing(stderr.String()),
		Cwd:    dir,
	}

	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = -1
	case errors.Is(ctx.Err(), context.Canceled):
		res.Cancelled = true
		res.ExitCode = -1
	case runErr != nil:
		if status, ok := interp.IsExitStatus(runErr); ok {
			res.ExitCode = int(status)
		} else {
			if res.Stderr == "" {
				res.Stderr = runErr.Error()
			}
			res.ExitCode = 1
		}
	}
	return res
}

// mvdanDevNull is a no-op ReadWriteCloser used for /dev/null and nul redirects.
type mvdanDevNull struct{}

func (mvdanDevNull) Read(_ []byte) (int, error)  { return 0, io.EOF }
func (mvdanDevNull) Write(p []byte) (int, error) { return len(p), nil }
func (mvdanDevNull) Close() error                { return nil }
