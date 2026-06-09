package bash

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errShellExited = errors.New("shell exited during command")
	errTimeout     = errors.New("command timed out")
	errCancelled   = errors.New("command cancelled")
)

type cmdResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Cwd       string
	TimedOut  bool
	Cancelled bool

	// Optional enrichment (see Module.enrich); zero values are omitted downstream.
	DurationMs   int64
	FilesChanged []string
	FilesNote    string
	Git          *gitInfo
}

type pending struct {
	out, errb *boundedBuf
	marker    string

	mu       sync.Mutex
	exit     int
	cwd      string
	outDone  bool
	errDone  bool
	finished bool
	done     chan struct{}
}

func (p *pending) markOut(exit int, cwd string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exit, p.cwd, p.outDone = exit, cwd, true
	p.maybeFinish()
}

func (p *pending) markErr() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errDone = true
	p.maybeFinish()
}

func (p *pending) maybeFinish() {
	if p.outDone && p.errDone && !p.finished {
		p.finished = true
		close(p.done)
	}
}

func (p *pending) snapshot() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit, p.cwd
}

// shell is a persistent shell process for one session. Commands are fed to its
// stdin and framed by an unguessable sentinel so the reader goroutines can
// attribute output and recover the exit code + $PWD. State (cwd, env, vars,
// functions, sourced scripts) persists across calls because it is one long-lived
// shell. run() is serialized: one command at a time.
type shell struct {
	kind      string
	marker    string
	outPrefix string // non-empty (PowerShell): only lines with this prefix are real output
	maxOut    int

	cmd   *exec.Cmd
	stdin io.WriteCloser

	runMu sync.Mutex
	cur   atomic.Pointer[pending]

	mu       sync.Mutex
	closed   bool
	lastUsed time.Time
	exited   chan struct{}
}

func newMarker() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "__DGT_" + hex.EncodeToString(b) + "__"
}

func detectShell(prefer string) (kind, path string, err error) {
	names := make([]string, 0, 5)
	if prefer != "" {
		names = append(names, prefer)
	}
	if runtime.GOOS == "windows" {
		// Bash FIRST on Windows now that execution is ONE-SHOT (each command is
		// its own short-lived process). The old msys2 fork-wedge ("cygheap read
		// copy failed") only struck a LONG-LIVED persistent bash REPL — a one-shot
		// `bash -c` spawns, runs, exits, so it never accumulates the fork state
		// that wedged. Preferring real bash means the agent's bash runs NATIVELY
		// (no `&&`→PowerShell / `2>/dev/null` / `export` translation, no frame) —
		// the whole class of shell-mismatch bugs disappears. PowerShell stays as
		// the fallback only when no real bash is present.
		names = append(names, "bash", "sh", "pwsh", "powershell")
	} else {
		names = append(names, "bash", "sh")
	}
	for _, name := range names {
		if p := lookShell(name); p != "" {
			return shellKind(p), p, nil
		}
	}
	return "", "", errors.New("no shell found on PATH (need bash, sh, or PowerShell)")
}

// lookShell resolves a shell name to an executable path. On Windows, asking for
// `bash` AVOIDS the WSL launcher (C:\Windows\System32\bash.exe) — which aborts
// with "execvpe(/bin/bash) failed" when no WSL distro is installed — and finds
// the real Git-for-Windows (msys2) bash instead. Returns "" when not found, so
// detection falls through to the next candidate (sh, then PowerShell).
func lookShell(name string) string {
	if runtime.GOOS == "windows" && strings.EqualFold(name, "bash") {
		return gitBashWindows() // never the WSL stub
	}
	if p, e := exec.LookPath(name); e == nil {
		return p
	}
	return ""
}

// gitBashWindows returns the path to a real Git-for-Windows / msys2 bash, or ""
// — explicitly skipping the System32 WSL launcher.
func gitBashWindows() string {
	for _, c := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\usr\bin\bash.exe`,
	} {
		if isExecFile(c) {
			return c
		}
	}
	// Derive from `git` on PATH : <Git>\cmd\git.exe → <Git>\bin\bash.exe.
	if gp, e := exec.LookPath("git"); e == nil {
		base := filepath.Dir(filepath.Dir(gp))
		for _, sub := range []string{`bin\bash.exe`, `usr\bin\bash.exe`} {
			if c := filepath.Join(base, sub); isExecFile(c) {
				return c
			}
		}
	}
	// A `bash` on PATH that is NOT the WSL System32 stub.
	if p, e := exec.LookPath("bash"); e == nil && !isWSLBash(p) {
		return p
	}
	return ""
}

// isWSLBash reports whether p is the Windows WSL launcher rather than a real
// bash binary (it lives at System32\bash.exe / Sysnative\bash.exe).
func isWSLBash(p string) bool {
	lp := strings.ToLower(filepath.Clean(p))
	return strings.HasSuffix(lp, `\system32\bash.exe`) || strings.HasSuffix(lp, `\sysnative\bash.exe`)
}

func isExecFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func shellKind(p string) string {
	b := strings.ToLower(filepath.Base(p))
	switch {
	case strings.Contains(b, "bash"):
		return "bash"
	case strings.Contains(b, "pwsh"), strings.Contains(b, "powershell"):
		return "powershell"
	default:
		return "sh"
	}
}

// psChain rewrites bash-style `&&` / `||` chaining into PowerShell. Windows
// PowerShell 5.1 — the only PowerShell present on most Windows hosts — does NOT
// support those operators (pwsh 7 does, and detectShell prefers it when found),
// so `cd app && npm install` is a hard parse error there. LLMs emit `&&`
// constantly, so instead of hoping the model avoids it we translate:
//
//	A && B  ->  A; if ($?) { B }
//	A || B  ->  A; if (-not $?) { B }
//
// folded right so chains nest correctly (`a && b && c`). Operators inside single
// or double quotes are left untouched, and a single `&`/`|` (background / pipe)
// is never split. A command with no top-level `&&`/`||` is returned verbatim.
func psChain(command string) string {
	type seg struct{ text, op string } // op precedes this segment ("" for first)
	var segs []seg
	var cur strings.Builder
	op := ""
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			cur.WriteByte(c)
			continue
		}
		if (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c {
			segs = append(segs, seg{strings.TrimSpace(cur.String()), op})
			cur.Reset()
			if c == '&' {
				op = "&&"
			} else {
				op = "||"
			}
			i++
			continue
		}
		cur.WriteByte(c)
	}
	segs = append(segs, seg{strings.TrimSpace(cur.String()), op})
	if len(segs) == 1 {
		return command
	}
	expr := segs[len(segs)-1].text
	for i := len(segs) - 1; i >= 1; i-- {
		prev := segs[i-1].text
		switch segs[i].op {
		case "&&":
			expr = prev + "; if ($?) { " + expr + " }"
		case "||":
			expr = prev + "; if (-not $?) { " + expr + " }"
		default:
			expr = prev + "; " + expr
		}
	}
	return expr
}

// psNulSink rewrites the null-device redirect targets that LLMs emit by reflex
// into PowerShell's `$null` sink :
//
//	bash : 2>/dev/null  >/dev/null  >/dev/null 2>&1
//	cmd  : 2>nul        >nul        1>nul
//
// On PowerShell BOTH forms are a trap : `>nul` opens a FILE named `nul` (a
// reserved device → ".NET FileStream … com1:/lpt1:"), and `>/dev/null` opens the
// path `C:\dev\null` → "Could not find a part of the path". The agent's bash
// (and CMD) muscle-memory is overwhelming, so we translate — the same way psChain
// translates `&&`. Targets inside single/double quotes are untouched, and a real
// path (`>nul.txt`, `>/dev/null.bak`) is NOT rewritten : only a bare `nul` or
// `/dev/null` token immediately followed by a redirect boundary.
func psNulSink(command string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if c == '>' {
			b.WriteByte(c)
			j := i + 1
			for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
				j++
			}
			if n := matchNullTarget(command, j); n > 0 {
				b.WriteString(command[i+1 : j]) // preserve any spaces after '>'
				b.WriteString("$null")
				i = j + n - 1 // skip the matched target (loop's i++ lands past it)
				continue
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// matchNullTarget reports the length of a null-device redirect target at s[j:]
// (`/dev/null` or `nul`), or 0 if none. The match must end at a redirect
// boundary so real paths (`nul.txt`, `/dev/null.bak`) are left alone.
func matchNullTarget(s string, j int) int {
	for _, tok := range []string{"/dev/null", "nul"} {
		end := j + len(tok)
		if end <= len(s) && strings.EqualFold(s[j:end], tok) &&
			(end == len(s) || isRedirBoundary(s[end])) {
			return len(tok)
		}
	}
	return 0
}

func isRedirBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '|', '&', ';', ')', '<', '>':
		return true
	}
	return false
}

// psEnv translates the bash environment-variable idioms an LLM emits into
// PowerShell, per top-level statement :
//
//	export NAME=value        ->  $env:NAME='value'
//	NAME=value some-command  ->  $env:NAME='value'; some-command   (inline env)
//
// Only SIMPLE values are translated (already-quoted values are kept verbatim;
// bare values are single-quoted). A value that references a shell variable
// (`$FOO`) is left untouched so the bashism guard can flag it with a clear hint
// rather than risk a wrong rewrite. Runs before psChain so `&&`/`||` chaining is
// handled afterwards.
func psEnv(command string) string {
	parts, seps := splitStatements(command)
	for i := range parts {
		parts[i] = translateEnvStmt(parts[i])
	}
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if i < len(seps) {
			b.WriteString(seps[i])
		}
	}
	return b.String()
}

// splitStatements splits on top-level `;`, `&&`, `||` and newlines (quote- and
// paren-aware), returning the segments and the separators between them
// (len(seps) == len(parts)-1) so the original string can be rebuilt exactly.
func splitStatements(command string) (parts, seps []string) {
	var cur strings.Builder
	var quote byte
	depth := 0
	flush := func(sep string) {
		parts = append(parts, cur.String())
		cur.Reset()
		if sep != "" {
			seps = append(seps, sep)
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch {
			case c == '\'' || c == '"':
				quote = c
				cur.WriteByte(c)
			case c == '(' || c == '{':
				depth++
				cur.WriteByte(c)
			case c == ')' || c == '}':
				if depth > 0 {
					depth--
				}
				cur.WriteByte(c)
			case depth == 0 && (c == ';' || c == '\n'):
				flush(string(c))
		case depth == 0 && (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c:
			flush(command[i : i+2])
			i++
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts, seps
}

func translateEnvStmt(seg string) string {
	leftTrimmed := strings.TrimLeft(seg, " \t")
	lead := seg[:len(seg)-len(leftTrimmed)]
	core := strings.TrimRight(leftTrimmed, " \t")
	trail := leftTrimmed[len(core):]

	if strings.HasPrefix(core, "export ") {
		rest := strings.TrimLeft(core[len("export "):], " \t")
		if name, val, ok := splitAssign(rest); ok && simpleEnvValue(val) {
			return lead + "$env:" + name + "=" + quoteEnvValue(val) + trail
		}
		return seg
	}
	if name, val, cmd, ok := splitInlineAssign(core); ok && simpleEnvValue(val) {
		return lead + "$env:" + name + "=" + quoteEnvValue(val) + "; " + cmd + trail
	}
	return seg
}

// splitAssign parses `NAME=VALUE` (VALUE may be empty / contain spaces).
func splitAssign(s string) (name, val string, ok bool) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = s[:eq]
	if !isEnvName(name) {
		return "", "", false
	}
	return name, s[eq+1:], true
}

// splitInlineAssign parses `NAME=VALUE command…` : the first whitespace token
// must be a bare `NAME=VALUE` (no spaces in the value) and a command must
// follow. Quoted/space-bearing inline values are left untranslated.
func splitInlineAssign(s string) (name, val, cmd string, ok bool) {
	sp := strings.IndexAny(s, " \t")
	if sp <= 0 {
		return "", "", "", false
	}
	first, rest := s[:sp], strings.TrimLeft(s[sp:], " \t")
	if rest == "" {
		return "", "", "", false
	}
	n, v, k := splitAssign(first)
	if !k {
		return "", "", "", false
	}
	return n, v, rest, true
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// simpleEnvValue is true for a value we can translate without risk : no shell
// variable reference and no command substitution.
func simpleEnvValue(v string) bool {
	return !strings.ContainsAny(v, "$`")
}

func quoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
			return v // already quoted — keep verbatim
		}
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func shellArgs(kind string) []string {
	switch kind {
	case "bash":
		return []string{"--norc", "--noprofile"}
	case "powershell":
		return []string{"-NoProfile", "-NoLogo", "-NonInteractive"}
	default:
		return nil
	}
}

// coreutilUnshadow lists the command names whose built-in PowerShell ALIAS
// (rm→Remove-Item, ls→Get-ChildItem, curl→Invoke-WebRequest …) shadows a real
// Unix coreutil of the same name shipped on the host (e.g. Git's usr\bin). The
// alias takes a different flag syntax than the LLM expects, so `rm -rf`, `ls
// -la`, `cat f | head`, `curl -s URL` fail with a cryptic "parameter cannot be
// found" instead of doing what the model intends. We drop each alias at shell
// startup — but ONLY when a real executable of that name exists on PATH, so a
// host without the coreutils keeps the alias (dropping it would turn `rm` into
// "not recognized", which is worse). The result: the agent's Unix muscle-memory
// just works, while everything else stays native PowerShell.
var coreutilUnshadow = []string{
	"rm", "ls", "cat", "cp", "mv", "sort", "curl", "wget",
	"tee", "sleep", "diff", "kill", "ps", "tr", "cut",
}

func warmupCmd(kind string) string {
	if kind == "powershell" {
		// Unshadow coreutils (see coreutilUnshadow). The guard makes every
		// removal safe + idempotent, and SilentlyContinue means this can never
		// throw — so it doubles as the shell round-trip warmup. Runs in the
		// session's global scope (dot-sourced by frame), so it persists for all
		// subsequent commands on this persistent shell.
		names := "'" + strings.Join(coreutilUnshadow, "','") + "'"
		return "foreach ($n in " + names + ") { " +
			"if ((Get-Command $n -CommandType Application -ErrorAction SilentlyContinue) -and (Test-Path ('Alias:'+$n))) { " +
			"Remove-Item ('Alias:'+$n) -Force -ErrorAction SilentlyContinue } }"
	}
	return ":"
}

// frame wraps the command in a group with stdin redirected from /dev/null so a
// command that reads stdin (cat, read, an interactive installer) cannot consume
// the framing bytes that follow on the shell's own stdin. The group runs in the
// current shell — so cd/export/vars still persist — then we emit an exit+pwd
// marker on stdout and a completion marker on stderr.
func frame(kind, command, marker string) string {
	if kind == "powershell" {
		// PowerShell runs as an interactive REPL (the only mode that executes
		// stdin incrementally), which echoes the prompt + every command. So we
		// tag the command's real output with an unguessable prefix and keep only
		// tagged lines (the reader drops everything else). $LASTEXITCODE is reset
		// first because cmdlets don't update it (it would otherwise be stale from
		// a previous native command). $PWD.Path persists across commands.
		// The command is base64-encoded (no quoting/injection hazard) and run via
		// Invoke-Expression, which executes in the CURRENT scope — so $vars,
		// functions and cd persist across calls. Its merged output is tagged and
		// streamed; everything else (echoed prompt/commands) is dropped.
		pfx := marker + "|"
		b64 := base64.StdEncoding.EncodeToString([]byte(command))
		// Dot-source (`.`) the decoded command so it runs in the CURRENT scope —
		// cd, $vars, functions and module imports persist across calls (the call
		// operator `&` would sandbox them in a child scope). `2>&1` MERGES the
		// command's error stream, which for NATIVE commands (npm, npx, git, node)
		// is the only way their stderr is captured at all — without it a failing
		// `npx create-react-app` returns exit 1 with NO text and the agent loops
		// blind. Native stderr arrives as ErrorRecords, so we flatten each to its
		// raw message (.ToString()) BEFORE Out-String, dropping PowerShell's
		// "At line:/CategoryInfo/FullyQualifiedErrorId" boilerplate. ErrorAction
		// Continue keeps a cmdlet error non-terminating so the marker always emits.
		// try/catch (single statement — no REPL multi-line buffering, and it does
		// NOT create a scope so the dot-source still persists state) surfaces a
		// TERMINATING error too: a syntax error like bash `&&` makes
		// [scriptblock]::Create throw, which otherwise printed to the raw console
		// and reported exit 0 (a parse failure masquerading as success). We emit
		// the exception message as tagged output and force a non-zero exit.
		return "$LASTEXITCODE = 0; $ErrorActionPreference = 'Continue'\n" +
			"try { . ([scriptblock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + b64 + "')))) 2>&1 | " +
			"ForEach-Object { if ($_ -is [System.Management.Automation.ErrorRecord]) { $_.ToString() } else { $_ } } | " +
			"Out-String -Stream -Width 4096 | ForEach-Object { [Console]::Out.WriteLine('" + pfx + "' + $_) } } " +
			"catch { [Console]::Out.WriteLine('" + pfx + "' + $_.Exception.Message); if (($null -eq $LASTEXITCODE) -or ($LASTEXITCODE -eq 0)) { $LASTEXITCODE = 1 } }\n" +
			"$__dgt_rc = $LASTEXITCODE; if ($null -eq $__dgt_rc) { $__dgt_rc = 0 }; if (-not $?) { if ($__dgt_rc -eq 0) { $__dgt_rc = 1 } }\n" +
			"[Console]::Out.WriteLine(\"" + marker + " $__dgt_rc $($PWD.Path)\")\n" +
			"[Console]::Error.WriteLine('" + marker + "')\n"
	}
	return "{ " + command + "\n} </dev/null\n" +
		"__dgt_rc=$?; printf '\\n%s %d %s\\n' '" + marker + "' \"$__dgt_rc\" \"$PWD\"; " +
		"printf '%s\\n' '" + marker + "' 1>&2\n"
}

func parseMarker(line, marker string) (int, string, bool) {
	if !strings.HasPrefix(line, marker+" ") {
		return 0, "", false
	}
	rest := line[len(marker)+1:]
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) < 2 {
		return 0, "", false
	}
	exit, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", false
	}
	return exit, parts[1], true
}

func newShell(kind, path, dir string, env []string, maxOut int) (*shell, error) {
	s := &shell{kind: kind, marker: newMarker(), maxOut: maxOut, exited: make(chan struct{}), lastUsed: time.Now()}
	if kind == "powershell" {
		s.outPrefix = s.marker + "|"
	}
	cmd := exec.Command(path, shellArgs(kind)...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	setSysProc(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.cmd = cmd
	s.stdin = stdin

	var rwg sync.WaitGroup
	rwg.Add(2)
	go func() { defer rwg.Done(); s.readLoop(stdout, false) }()
	go func() { defer rwg.Done(); s.readLoop(stderr, true) }()
	go func() { rwg.Wait(); _ = cmd.Wait(); close(s.exited) }()

	if kind == "bash" {
		_, _ = io.WriteString(stdin, "shopt -s expand_aliases 2>/dev/null\n")
	}
	res, err := s.run(context.Background(), warmupCmd(kind), 10*time.Second)
	if err != nil || res.TimedOut || res.ExitCode != 0 {
		s.close()
		if err == nil {
			err = fmt.Errorf("warmup incomplete (timed_out=%v exit=%d)", res.TimedOut, res.ExitCode)
		}
		return nil, fmt.Errorf("shell warmup failed: %w", err)
	}
	return s, nil
}

const (
	shellStartAttempts = 5
	shellStartBackoff  = 50 * time.Millisecond
)

// newShellResilient starts a persistent shell, retrying transient startup
// deaths. On Windows the Git-Bash (msys2) fork() emulation intermittently
// aborts a freshly-spawned shell with "cygheap read copy failed"
// (ERROR_PARTIAL_COPY) — an ASLR/heap-copy race that has nothing to do with the
// command, so the very next exec almost always succeeds. We retry only that
// signature (the shell exited during warmup, surfaced as errShellExited /
// errTimeout); a permanent error such as a bad shell path breaks out at once.
func newShellResilient(kind, path, dir string, env []string, maxOut int) (*shell, error) {
	var err error
	for attempt := 0; attempt < shellStartAttempts; attempt++ {
		var sh *shell
		if sh, err = newShell(kind, path, dir, env, maxOut); err == nil {
			return sh, nil
		}
		if !errors.Is(err, errShellExited) && !errors.Is(err, errTimeout) {
			break
		}
		time.Sleep(shellStartBackoff)
	}
	return nil, err
}

// maxScanLine bounds a single output line. A command can emit a line with NO
// newline for megabytes (a minified bundle, `cat` of a one-line file); the
// default bufio.Scanner aborts past its max token and the completion MARKER —
// a later line — is then never read, wedging the command until timeout.
// cappedLineSplit instead chops an over-long line into bounded chunks so the
// marker line is always reached and memory stays bounded (boundedBuf truncates
// the chunks anyway).
const maxScanLine = 1 << 20 // 1 MB ; marker lines are tiny, never split

func cappedLineSplit(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil // a full line (newline dropped)
	}
	if len(data) >= maxScanLine { // no newline yet and the line is too long: emit a chunk
		return maxScanLine, data[:maxScanLine], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil // need more data
}

func (s *shell) readLoop(r io.Reader, isErr bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxScanLine+64*1024)
	sc.Split(cappedLineSplit)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		p := s.cur.Load()
		if p == nil {
			continue
		}
		if isErr {
			if line == p.marker {
				p.markErr()
				continue
			}
			_, _ = p.errb.Write([]byte(line + "\n"))
			continue
		}
		if exit, cwd, ok := parseMarker(line, p.marker); ok {
			p.markOut(exit, cwd)
			continue
		}
		if s.outPrefix != "" {
			// PowerShell: only our tagged lines are real output; the REPL's
			// echoed prompt + command lines have no prefix → drop them.
			if rest, ok := strings.CutPrefix(line, s.outPrefix); ok {
				_, _ = p.out.Write([]byte(rest + "\n"))
			}
			continue
		}
		_, _ = p.out.Write([]byte(line + "\n"))
	}
}

func (s *shell) run(ctx context.Context, command string, timeout time.Duration) (cmdResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return cmdResult{}, errors.New("shell is closed")
	}
	s.lastUsed = time.Now()
	s.mu.Unlock()

	p := &pending{
		out:    newBoundedBuf(s.maxOut),
		errb:   newBoundedBuf(s.maxOut),
		marker: s.marker,
		done:   make(chan struct{}),
	}
	s.cur.Store(p)
	defer s.cur.Store(nil)

	if _, err := io.WriteString(s.stdin, frame(s.kind, command, s.marker)); err != nil {
		return cmdResult{}, fmt.Errorf("write to shell: %w", err)
	}

	var tctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		tctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		tctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	select {
	case <-p.done:
		exit, cwd := p.snapshot()
		return cmdResult{Stdout: trimTrailing(p.out.String()), Stderr: trimTrailing(p.errb.String()), ExitCode: exit, Cwd: cwd}, nil
	case <-s.exited:
		exit, cwd := p.snapshot()
		return cmdResult{Stdout: trimTrailing(p.out.String()), Stderr: trimTrailing(p.errb.String()), ExitCode: exit, Cwd: cwd}, errShellExited
	case <-tctx.Done():
		// Distinguish a caller/turn cancellation (the user hit stop, or the
		// background task was cancelled) from our own deadline firing.
		cancelled := ctx.Err() != nil
		s.killAll()
		time.Sleep(150 * time.Millisecond)
		exit, cwd := p.snapshot()
		res := cmdResult{Stdout: trimTrailing(p.out.String()), Stderr: trimTrailing(p.errb.String()), ExitCode: exit, Cwd: cwd, TimedOut: !cancelled, Cancelled: cancelled}
		if res.ExitCode == 0 {
			res.ExitCode = 124
		}
		if cancelled {
			return res, errCancelled
		}
		return res, errTimeout
	}
}

// killAll reaps the shell and its entire process tree. A timed-out or cancelled
// command therefore leaves nothing running; the session shell is gone and the
// module starts a fresh one on the next call (state is intentionally reset).
func (s *shell) killAll() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		killProcessTree(s.cmd.Process.Pid)
		_ = s.cmd.Process.Kill()
	}
	_ = s.stdin.Close()
}

func (s *shell) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *shell) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastUsed)
}

func (s *shell) close() {
	s.killAll()
	select {
	case <-s.exited:
	case <-time.After(2 * time.Second):
	}
}

func trimTrailing(s string) string {
	return strings.TrimRight(s, "\r\n")
}
