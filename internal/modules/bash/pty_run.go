package bash

import (
	"context"
	"io"
	"regexp"
	"strings"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
)

// ansiEscape matches ANSI/VT100 escape sequences so they can be stripped from
// PTY output before the LLM sees it — the LLM needs clean text, not colour codes.
var ansiEscape = regexp.MustCompile(
	`\x1b(\[[0-9;]*[mGKHFABCDJSTPl h]|` +
		`\][^\x07\x1b]*(?:\x07|\x1b\\)|` +
		`[()][AB012]|` +
		`[=>78MNOPcH]|` +
		`\[[0-9;]*[a-zA-Z])`)

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// runWithPTY runs command inside a pseudo-terminal (PTY on Unix, ConPTY on
// Windows). The PTY makes isatty() return true so docker -it, ssh, winget and
// any program that requires a real console work correctly.
//
// stdout and stderr are merged (PTY has a single output stream); ANSI escape
// sequences are stripped before returning. Falls back to runDetached silently
// if PTY allocation fails (headless CI, containers without /dev/ptmx, etc.).
func runWithPTY(ctx context.Context, kind, shellPath, command, dir string, env []string, maxOut int, timeout time.Duration) cmdResult {
	var cctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		cctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	pt, err := gopty.New()
	if err != nil {
		// PTY unavailable (headless container, CI without /dev/ptmx, etc.).
		res, _ := runDetached(ctx, kind, shellPath, command, dir, env, maxOut, "", timeout, 0)
		if res.Stderr != "" {
			res.Stderr = "[pty unavailable, ran without tty] " + res.Stderr
		} else {
			res.Stdout = "[pty unavailable, ran without tty] " + res.Stdout
		}
		return res
	}
	defer pt.Close()

	// Wide terminal so wrapped output is minimal.
	_ = pt.Resize(220, 50)

	// Build the Cmd through the PTY so it gets the terminal attribute.
	var cmd *gopty.Cmd
	switch kind {
	case "powershell", "pwsh":
		cmd = pt.CommandContext(cctx, shellPath, "-NonInteractive", "-Command", command)
	default: // bash, sh
		cmd = pt.CommandContext(cctx, shellPath, "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		res, _ := runDetached(ctx, kind, shellPath, command, dir, env, maxOut, "", timeout, 0)
		return res
	}

	// Drain PTY output into a bounded buffer concurrently with Wait.
	buf := newBoundedBuf(maxOut)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		b := make([]byte, 4096)
		for {
			n, rerr := pt.Read(b)
			if n > 0 {
				_, _ = buf.Write(b[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Wait for the process, respecting context cancellation.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var exitCode int
	var timedOut, cancelled bool

	select {
	case waitErr := <-waitCh:
		if waitErr != nil {
			if ps := cmd.ProcessState; ps != nil {
				exitCode = ps.ExitCode()
				if exitCode < 0 {
					exitCode = 1 // signal kill
				}
			} else {
				exitCode = 1
			}
		}
		// Give the drain goroutine a moment to flush buffered PTY output.
		select {
		case <-drainDone:
		case <-time.After(300 * time.Millisecond):
		}

	case <-cctx.Done():
		if ctx.Err() != nil {
			cancelled = true
		} else {
			timedOut = true
		}
		// Close PTY to unblock the drain goroutine, then wait for the process.
		pt.Close()
		<-waitCh
		exitCode = 130
	}

	// The PTY merges stdout+stderr into a single stream — return it all as Stdout.
	raw := buf.String()
	out := stripANSI(raw)
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "\n")

	return cmdResult{
		Stdout:    trimTrailing(out),
		Stderr:    "",
		ExitCode:  exitCode,
		Cwd:       dir,
		TimedOut:  timedOut,
		Cancelled: cancelled,
	}
}

// Ensure the io import is used (Read returns io.EOF).
var _ = io.EOF
