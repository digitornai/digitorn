// Package goshell runs shell commands with a pure-Go bash-compatible
// interpreter (mvdan.cc/sh). It needs no external shell, no DLL, no install:
// the shell logic IS the daemon, identical on Windows, Linux and macOS. The
// agent's bash runs natively — no PowerShell translation, no "is bash
// installed". External programs (git, node, python, …) are exec'd from PATH
// like any shell; on a Windows host without GNU coreutils, an embedded busybox
// dir is prepended to PATH so sed/awk/grep/… resolve.
package goshell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Run parses and executes script in dir with the given environment and IO.
// It returns the shell exit code. A parse error returns (2, err); a runtime
// setup error returns (1, err); a non-zero command exit returns (code, nil) —
// the exit code is data, not a Go error.
func Run(ctx context.Context, script, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if runtime.GOOS == "windows" {
		// An agent types `cd C:\Users\x` constantly; POSIX escaping would eat the
		// backslashes (→ `C:Usersx`). Normalize an unquoted drive path to forward
		// slashes (which address the same path) so the command the agent obviously
		// meant just works. Quoted text is untouched.
		script = normalizeWinPaths(script)
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		return 2, err
	}

	// Pipeline stages share one stderr (and may share stdout) and run
	// concurrently, so the writers MUST be safe for concurrent Write — a plain
	// bytes.Buffer is not. Serialize every Write through a mutex.
	runner, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(withBusybox(env)...)),
		interp.StdIO(stdin, syncWriter(stdout), syncWriter(stderr)),
		interp.ExecHandler(treeKillExec),
	)
	if err != nil {
		return 1, err
	}
	runErr := runner.Run(ctx, prog)
	if runErr == nil {
		return 0, nil
	}
	var status interp.ExitStatus
	if errors.As(runErr, &status) {
		return int(status), nil
	}
	return 1, runErr
}

// treeKillExec replaces interp's default exec handler. It is identical except
// for two things: every external command is started in its OWN process group,
// and when the context is cancelled the WHOLE descendant tree is reaped — not
// just the direct child. Without this, cancelling a `npm run dev` (npm → node →
// workers) would kill npm but orphan the node server, which keeps holding its
// port. This restores the tree-reaping the subprocess backend had.
func treeKillExec(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(127)
	}
	cmd := exec.Cmd{
		Path:   path,
		Args:   args,
		Env:    execEnv(hc.Env),
		Dir:    hc.Dir,
		Stdin:  hc.Stdin,
		Stdout: hc.Stdout,
		Stderr: hc.Stderr,
	}
	setProcGroup(&cmd)

	if err = cmd.Start(); err == nil {
		pid := cmd.Process.Pid
		// Hard tree-kill on cancel. A dev server must release its port NOW; the
		// interpreter already surfaces the cancellation as the run error.
		stop := context.AfterFunc(ctx, func() { killProcTree(pid) })
		defer stop()
		err = cmd.Wait()
	}

	switch e := err.(type) {
	case *exec.ExitError:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return interp.ExitStatus(e.ExitCode())
	case *exec.Error:
		fmt.Fprintf(hc.Stderr, "%v\n", e)
		return interp.ExitStatus(127)
	default:
		return err
	}
}

// execEnv mirrors interp's own (unexported) env flattening: only exported string
// variables reach the child's environment, matching what the default handler
// would have passed.
func execEnv(env expand.Environ) []string {
	list := make([]string, 0, 64)
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
		return true
	})
	return list
}

// normalizeWinPaths rewrites Windows path backslashes to forward slashes before
// the script is parsed. A POSIX shell treats `\` as an escape, so an UNQUOTED
// `C:\Users\x` or `src\index.js` collapses (→ `C:Usersx`, `srcindex.js`) and the
// path is lost — yet an LLM agent on Windows types exactly that constantly.
// Forward slashes address the identical path, so we honor what the agent meant.
//
// A `\` is rewritten ONLY when it is acting as a path separator (it precedes a
// path character). A `\` that escapes a space or a shell metacharacter —
// `cd My\ Documents`, `\$VAR`, `grep \( f` — is an INTENTIONAL escape and is left
// exactly as-is. Quoted text is never touched.
func normalizeWinPaths(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	var quote rune
	for i := 0; i < len(r); i++ {
		c := r[i]
		if quote != 0 {
			b.WriteRune(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			b.WriteRune(c)
		case '\\':
			if i+1 < len(r) && isPathChar(r[i+1]) {
				b.WriteByte('/') // path separator, not a shell escape
			} else {
				b.WriteRune(c) // intentional escape (\space, \$, \(, …) — keep
			}
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// isPathChar reports whether c can follow a backslash that is serving as a
// Windows path separator (alphanumerics, the usual filename punctuation, and a
// further slash for UNC `\\host` / mixed separators) — as opposed to a shell
// metacharacter or space, which would mean the backslash is an intentional
// escape.
func isPathChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '.', '-', '_', '~', '\\', '/':
		return true
	}
	return false
}

// TrailingBackground reports whether the script's LAST top-level statement is a
// background job (ends in a single `&`). Such a job is orphaned the moment the
// foreground command returns — detached from the daemon, untracked, still
// holding its port. Parse-based, so `&&`, a quoted `&`, and `… & wait` are all
// judged correctly (that last one ends in `wait`, not `&`). A parse error
// returns false so the normal run path surfaces the real syntax error instead.
func TrailingBackground(script string) bool {
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil || prog == nil || len(prog.Stmts) == 0 {
		return false
	}
	return prog.Stmts[len(prog.Stmts)-1].Background
}

// HasBusybox reports whether the embedded busybox coreutils are available as a
// PATH fallback on this host (Windows builds where extraction succeeded). Used
// to describe the live backend to the agent.
func HasBusybox() bool { return busyboxDir() != "" }

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// syncWriter wraps w so concurrent pipeline stages can't corrupt it. nil passes
// through (interp then uses its default).
func syncWriter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &lockedWriter{w: w}
}

// withBusybox prepends the embedded busybox coreutils dir to PATH so the
// agent's sed/awk/grep/xargs — and busybox's own sub-execs — resolve on a host
// that lacks GNU coreutils. No-op when no busybox is embedded (every non-
// Windows host, and any Windows host where extraction failed).
func withBusybox(env []string) []string {
	dir := busyboxDir()
	if dir == "" {
		return env
	}
	out := make([]string, 0, len(env)+2)
	pathSet, globSet := false, false
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			switch {
			case strings.EqualFold(kv[:i], "PATH"):
				out = append(out, "PATH="+dir+string(os.PathListSeparator)+kv[i+1:])
				pathSet = true
				continue
			case kv[:i] == "BB_GLOBBING":
				// goshell already performed POSIX globbing; busybox-w32 must NOT
				// re-glob its argv (else `find -name '*.go'` gets the * expanded
				// to filenames and the tool breaks).
				out = append(out, "BB_GLOBBING=0")
				globSet = true
				continue
			}
		}
		out = append(out, kv)
	}
	if !pathSet {
		out = append(out, "PATH="+dir)
	}
	if !globSet {
		out = append(out, "BB_GLOBBING=0")
	}
	return out
}
