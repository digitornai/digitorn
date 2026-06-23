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
	errShellExited     = errors.New("shell exited during command")
	errTimeout         = errors.New("command timed out")
	errCancelled       = errors.New("command cancelled")
	errWaitingForInput = errors.New("command waiting for input")
)

type cmdResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Cwd       string
	TimedOut  bool
	Cancelled bool

	WaitingForInput bool

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

type shell struct {
	kind      string
	marker    string
	outPrefix string
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
		names = append(names, "bash", "sh", "pwsh", "powershell")
	} else {
		names = append(names, "bash", "sh")
	}
	for _, name := range names {
		if p := lookShell(name); p != "" {
			return shellKind(p), p, nil
		}
	}
	if runtime.GOOS == "windows" {
		if wslBash := detectWSLBash(); wslBash != "" {
			return "bash", wslBash, nil
		}
	}
	return "", "", errors.New("no shell found on PATH (need bash, sh, PowerShell, or WSL)")
}

func detectWSLBash() string {
	wslExe, err := exec.LookPath("wsl.exe")
	if err != nil {
		return ""
	}
	cmd := exec.Command(wslExe, "--list", "--quiet")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return ""
	}
	return wslExe
}

func lookShell(name string) string {
	if runtime.GOOS == "windows" && strings.EqualFold(name, "bash") {
		return gitBashWindows()
	}
	if p, e := exec.LookPath(name); e == nil {
		return p
	}
	return ""
}

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
	if gp, e := exec.LookPath("git"); e == nil {
		base := filepath.Dir(filepath.Dir(gp))
		for _, sub := range []string{`bin\bash.exe`, `usr\bin\bash.exe`} {
			if c := filepath.Join(base, sub); isExecFile(c) {
				return c
			}
		}
	}
	if p, e := exec.LookPath("bash"); e == nil && !isWSLBash(p) {
		return p
	}
	return ""
}

func detectWSLExe() string {
	wslExe, err := exec.LookPath("wsl.exe")
	if err != nil {
		return ""
	}
	out, err := exec.Command(wslExe, "--list", "--quiet").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return ""
	}
	return wslExe
}

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

func psChain(command string) string {
	type seg struct{ text, op string }
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
				b.WriteString(command[i+1 : j])
				b.WriteString("$null")
				i = j + n - 1
				continue
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

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

func simpleEnvValue(v string) bool {
	return !strings.ContainsAny(v, "$`")
}

func quoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
			return v
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

var coreutilUnshadow = []string{
	"rm", "ls", "cat", "cp", "mv", "sort", "curl", "wget",
	"tee", "sleep", "diff", "kill", "ps", "tr", "cut",
}

func warmupCmd(kind string) string {
	if kind == "powershell" {
		names := "'" + strings.Join(coreutilUnshadow, "','") + "'"
		return "foreach ($n in " + names + ") { " +
			"if ((Get-Command $n -CommandType Application -ErrorAction SilentlyContinue) -and (Test-Path ('Alias:'+$n))) { " +
			"Remove-Item ('Alias:'+$n) -Force -ErrorAction SilentlyContinue } }"
	}
	return ":"
}

func frame(kind, command, marker string) string {
	if kind == "powershell" {
		pfx := marker + "|"
		b64 := base64.StdEncoding.EncodeToString([]byte(command))
		return "$LASTEXITCODE = 0; $ErrorActionPreference = 'Continue'\n" +
			"$__dgt_savedIn = [Console]::In; [Console]::SetIn([System.IO.TextReader]::Null)\n" +
			"try { . ([scriptblock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + b64 + "')))) 2>&1 | " +
			"ForEach-Object { if ($_ -is [System.Management.Automation.ErrorRecord]) { $_.ToString() } else { $_ } } | " +
			"Out-String -Stream -Width 4096 | ForEach-Object { [Console]::Out.WriteLine('" + pfx + "' + $_) } } " +
			"catch { [Console]::Out.WriteLine('" + pfx + "' + $_.Exception.Message); if (($null -eq $LASTEXITCODE) -or ($LASTEXITCODE -eq 0)) { $LASTEXITCODE = 1 } } " +
			"finally { [Console]::SetIn($__dgt_savedIn) }\n" +
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

const maxScanLine = 1 << 20

func cappedLineSplit(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if len(data) >= maxScanLine {
		return maxScanLine, data[:maxScanLine], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
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
		cancelled := ctx.Err() != nil
		s.killAllCause(context.Cause(ctx))
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

func (s *shell) killAll() { s.killAllCause(nil) }

func (s *shell) killAllCause(cause error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		switch cause {
		case errSignalINT:
			signalProcessTree(s.cmd.Process.Pid, syscallSIGINT)
		case errSignalTERM:
			signalProcessTree(s.cmd.Process.Pid, syscallSIGTERM)
		default:
			killProcessTree(s.cmd.Process.Pid)
			_ = s.cmd.Process.Kill()
		}
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
