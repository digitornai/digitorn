// Package bash exposes a single command-execution tool. Every command runs as
// its own one-shot process from the workspace root (no persistent REPL), so a
// malformed command can't poison the next and the agent's bash runs natively —
// real Git Bash when present, otherwise the built-in pure-Go interpreter
// (goshell) plus embedded busybox coreutils, never PowerShell. Output is
// bounded with a truncation marker, a timeout or cancel reaps the whole process
// tree, and each result is enriched with cwd/duration/files-changed/git so the
// agent sees the effect of its command.
package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
	"github.com/mbathepaul/digitorn/internal/safego"
	"github.com/mbathepaul/digitorn/pkg/module"
)

type Config struct {
	Workdir     string   `json:"workdir" yaml:"workdir"`
	Shell       string   `json:"shell" yaml:"shell"`
	MaxOutput   int      `json:"max_output" yaml:"max_output"`
	TimeoutSecs int      `json:"timeout_seconds" yaml:"timeout_seconds"`
	IdleSecs    int      `json:"idle_seconds" yaml:"idle_seconds"`
	EnvAllow    []string `json:"env_allow" yaml:"env_allow"`
}

type Module struct {
	module.Base
	cfg  Config
	kind string
	path string

	// useGoShell is set when no real bash is on the host: commands then run
	// through the built-in pure-Go bash interpreter (goshell) instead of any
	// system shell. Self-contained, identical on every OS, and never PowerShell.
	useGoShell bool

	mu     sync.Mutex
	shells map[string]*shell

	stopJanitor chan struct{}
	janitorDone chan struct{}

	// collectCtx enriches each result with cwd/duration/files-changed/git so the
	// agent sees the effect of its command. On by default; DIGITORN_BASH_CONTEXT=0
	// turns it off. envInfo is the one-shot host snapshot baked into the tool
	// description. gitCache memoises per-workspace dirty counts (see gitContext).
	collectCtx bool
	envInfo    string
	gitMu      sync.Mutex
	gitCache   map[string]*gitInfo
}

func New() *Module {
	m := &Module{shells: map[string]*shell{}}
	m.Base = module.Base{
		ID:          "bash",
		Version:     "1.0.0",
		Description: "Run shell commands as one-shot processes from the workspace root.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
		DeclaredPermissions: []string{"bash.run"},
	}
	m.registerRunTool()
	return m
}

// registerRunTool (re)registers the `run` tool. Called once in New(), then
// again at the end of Init() once the host snapshot is known so the description
// carries it. RegisterTool overwrites by name, so the second call replaces the
// first. The wording is always one-shot/stateless — that is the truth for every
// active backend (goshell and the subprocess path both run each command fresh).
func (m *Module) registerRunTool() {
	desc := runDescCore + " " + runDescStateless + " " + runDescTail
	if m.envInfo != "" {
		desc += "\n\nThis host: " + m.envInfo + "."
	}
	m.RegisterTool(module.Tool{
		Name:         "run",
		Description:  desc,
		ToolPrompt:   runToolPrompt,
		Params:       runToolParams,
		Permissions:  []string{"bash.run"},
		RiskLevel:    tool.RiskHigh,
		Irreversible: true,
		Aliases:      []string{"bash", "sh", "shell"},
		Handler:      m.run,
	})
}

const (
	runDescCore = "Execute a shell command and return its stdout, stderr, exit code and working directory."

	runDescStateless = "Each command is its own one-shot process starting at the workspace root: shell state (`cd`, `export`, venv, functions) does NOT persist between calls — do setup and use in ONE command (e.g. `cd proj && npm install`, `source venv/bin/activate && pytest`)."

	runDescTail = "Commands run non-interactively (no TTY) — chain with `&&`/`|`/`;` and pass flags rather than relying on prompts. A command that never returns on its own — a dev server (`npm run dev`, `vite`, `flask run`), a watcher, `tail -f`, a REPL — MUST be started with `background_run`; do NOT run it in the foreground (it freezes the turn until the timeout) and do NOT append a trailing `&`. Backgrounded commands have NO timeout — they run until they finish or are cancelled. Use the foreground only for commands that terminate on their own (builds, installs, tests, git). Each result also reports the working directory, how long it took, any files the command changed, and the git branch/dirty status, so you can see the effect of your action without probing for it."
)

var runToolPrompt = "Use the shell for actions, not for inspecting files — prefer the dedicated tools: `read` over `cat`/`head`/`tail`, `grep` over `grep`/`rg`, `glob` over `find`, `edit` over `sed`/`awk`. They render better, are safer, and integrate with the UI. Reach for the shell to build, test, run, install, and drive git.\n" +
	"Quote paths with spaces; chain dependent steps in one call with `&&` so they fail fast.\n" +
	"Anything long-running or that never exits on its own goes through `background_run`, never the foreground and never a trailing `&`.\n" +
	"This is the highest-risk tool and edits/deletes are irreversible: before a destructive command (`rm -rf`, `git reset --hard`, `git push --force`, overwriting files) state what it will do and confirm it's intended. Never exfiltrate secrets or pipe credentials to the network."

var runToolParams = []tool.ParamSpec{
	{Name: "command", Type: "string", Description: "The shell command line to execute.", Required: true},
	{Name: "timeout_seconds", Type: "integer", Description: "Per-call timeout in seconds; the running command's process tree is killed on expiry. 0 = module default.", Default: 0},
	{Name: "input", Type: "string", Description: "Text fed to the command's stdin — use it to answer prompts or pipe data in (e.g. \"y\\n\"). The command runs as its own one-shot process.", Required: false},
}

// HasShell reports whether a usable POSIX shell was found at Init. Used by
// integration tests to skip when no shell is available on the host.
func (m *Module) HasShell() bool { return m.path != "" }

func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	if cfg != nil {
		raw, _ := json.Marshal(cfg)
		_ = json.Unmarshal(raw, &m.cfg)
	}
	if m.cfg.MaxOutput <= 0 {
		m.cfg.MaxOutput = 1 << 20
	}
	if m.cfg.TimeoutSecs <= 0 {
		m.cfg.TimeoutSecs = 120
	}
	if m.cfg.IdleSecs <= 0 {
		m.cfg.IdleSecs = 900
	}
	// An explicit override always wins (parity with CLAUDE_CODE_GIT_BASH_PATH):
	// point it at any bash.exe / sh and we use it verbatim.
	prefer := m.cfg.Shell
	if env := strings.TrimSpace(os.Getenv("DIGITORN_BASH_PATH")); env != "" {
		prefer = env
	}
	// Explicit opt-in to the built-in Go interpreter (shell: "goshell" / env
	// DIGITORN_BASH_PATH=goshell) — lets you exercise the no-host-bash path even
	// on a machine that HAS Git Bash.
	if strings.EqualFold(prefer, "goshell") || strings.EqualFold(prefer, "go") {
		m.useGoShell = true
		m.finalizeInit()
		return nil
	}
	// On Windows, goshell IS the shell — we do NOT delegate to a host Git Bash.
	// Git Bash drags an MSYS layer that silently rewrites native arguments
	// (`taskkill /F` → `F:/`, `/c/Users` → `C:\Users`), behaves differently across
	// installs/versions, and isn't guaranteed present at all. Our self-contained
	// interpreter behaves identically on every machine and has none of those
	// footguns. An explicit override (cfg.Shell / DIGITORN_BASH_PATH) is still
	// honored as an escape hatch for anyone who truly wants their own bash.
	if goruntime.GOOS == "windows" && prefer == "" {
		m.useGoShell = true
		m.finalizeInit()
		return nil
	}
	kind, path, err := detectShell(prefer)
	if err == nil {
		m.kind, m.path = kind, path
	}
	// Off Windows, a missing/non-bash shell still falls back to the built-in
	// interpreter rather than PowerShell — self-contained, identical everywhere,
	// in-process, with the agent's bash (arrays, [[ ]], $(), &&) running natively.
	if m.kind != "bash" {
		m.useGoShell = true
	}
	m.finalizeInit()
	return nil
}

// finalizeInit runs once the backend is decided: it enables result enrichment
// (unless DIGITORN_BASH_CONTEXT disables it), takes the one-shot host snapshot,
// and re-registers the run tool so its description matches the real backend.
func (m *Module) finalizeInit() {
	m.collectCtx = true
	if v := strings.TrimSpace(os.Getenv("DIGITORN_BASH_CONTEXT")); v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		m.collectCtx = false
	}
	m.envInfo = m.terminalSnapshot()
	m.registerRunTool()
}

func (m *Module) Start(ctx context.Context) error {
	if err := m.Base.Start(ctx); err != nil {
		return err
	}
	m.stopJanitor = make(chan struct{})
	m.janitorDone = make(chan struct{})
	go m.janitor()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	if m.stopJanitor != nil {
		close(m.stopJanitor)
		<-m.janitorDone
		m.stopJanitor = nil
	}
	m.mu.Lock()
	for k, sh := range m.shells {
		sh.close()
		delete(m.shells, k)
	}
	m.mu.Unlock()
	return m.Base.Stop(ctx)
}

func (m *Module) janitor() {
	defer close(m.janitorDone)
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	idle := time.Duration(m.cfg.IdleSecs) * time.Second
	for {
		select {
		case <-m.stopJanitor:
			return
		case <-t.C:
			safego.Run("bash.janitor", func() {
				m.mu.Lock()
				defer m.mu.Unlock() // deferred so a panic mid-reap can't leak the lock
				for k, sh := range m.shells {
					if sh.idle() > idle || sh.isClosed() {
						sh.close()
						delete(m.shells, k)
					}
				}
			})
		}
	}
}

type runParams struct {
	Command        string  `json:"command"`
	TimeoutSeconds flexInt `json:"timeout_seconds"`
	Input          string  `json:"input"`
}

// flexInt accepts a JSON number OR a string ("60", "60.0"), null, or "" — LLMs
// frequently send numeric params as strings, which a plain int rejects (and the
// whole tool call then fails). Unparseable input degrades to 0 (the default).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(int(v))
	return nil
}

type runResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Cwd       string `json:"cwd"`
	Shell     string `json:"shell"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`

	// Context attached so the agent sees the effect of its command without
	// spending a turn probing for it. All omitempty: a plain command stays terse.
	DurationMs   int64    `json:"duration_ms,omitempty"`
	FilesChanged []string `json:"files_changed,omitempty"`
	FilesNote    string   `json:"files_note,omitempty"`
	Git          *gitInfo `json:"git,omitempty"`
}

func (m *Module) run(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p runParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), nil
	}
	command := strings.TrimSpace(p.Command)
	if command == "" {
		return errResult(errors.New("command must not be empty")), nil
	}
	if err := checkCommand(command); err != nil {
		return errResult(err), nil
	}
	// A trailing `&` orphans the process (untracked, no notification, holds its
	// port) — force the managed background path instead.
	if msg := backgroundAmpHint(command); msg != "" {
		return errResult(errors.New(msg)), nil
	}
	// A server/watcher in the FOREGROUND pins the turn until the timeout (the loop
	// must never be blocked). Background dispatches are exempt — that IS the right
	// channel for a long-living process.
	if !tool.IsBackground(ctx) {
		if msg := foregroundServerHint(command); msg != "" {
			return errResult(errors.New(msg)), nil
		}
	}
	// Windows shell-dialect confusion: on a bash/sh shell (incl. the pure-Go
	// goshell) the model sometimes types cmd.exe syntax (`dir /B /S`, `copy`,
	// `del`). Reject with the bash equivalent instead of a cryptic exit-127.
	// A PowerShell target is exempt — it aliases these and has bashismHint.
	if m.useGoShell || m.kind != "powershell" {
		if msg := dosHint(command); msg != "" {
			return errResult(errors.New(msg)), nil
		}
	}

	// No real bash on this host → run the agent's bash in the built-in pure-Go
	// interpreter (never PowerShell). Self-contained and identical on every OS.
	if m.useGoShell {
		root := m.cfg.Workdir
		if pp, ok := workdir.PathPolicyFromContext(ctx); ok && pp.HasWorkdir() {
			root = pp.Root()
		}
		timeout := time.Duration(m.cfg.TimeoutSecs) * time.Second
		if p.TimeoutSeconds > 0 {
			timeout = time.Duration(p.TimeoutSeconds) * time.Second
		}
		if tool.IsBackground(ctx) && p.TimeoutSeconds <= 0 {
			timeout = 0
		}
		started := time.Now()
		res := runGoShell(ctx, command, root, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, p.Input, timeout)
		m.enrich(&res, root, started)
		workdir.NotifyFileChange(ctx) // a command may mutate the tree; shadow-repo git status is the arbiter
		return m.result(command, res, nil, timeout), nil
	}

	if m.path == "" {
		return errResult(errors.New("no shell available: bash or sh must be on PATH")), nil
	}
	// On Windows PowerShell 5.1 bash-style `&&`/`||` are parse errors; translate
	// the LLM's chaining into PowerShell so its natural output runs. Done once
	// here so both the foreground shell and the detached background path inherit
	// it. No-op for bash/sh and for commands without top-level `&&`/`||`.
	if m.kind == "powershell" {
		// Translate the agent's bash reflexes to PowerShell : env-var assignment
		// (export / inline NAME=val) first, then `&&`/`||` chaining, then the
		// null-device redirect (`2>/dev/null`, `2>nul`). Whatever bash-only syntax
		// survives (control-flow, source, $-valued export) gets a clear hint
		// instead of a cryptic parse exception the agent would loop on.
		command = psNulSink(psChain(psEnv(command)))
		if msg := bashismHint(command); msg != "" {
			return errResult(errors.New(msg)), nil
		}
		// One-shot loses the persistent shell's startup warmup, so drop the
		// coreutil-shadowing aliases (rm→Remove-Item …) per invocation — keeps
		// `rm -rf`/`cp`/`mv` working on the PowerShell fallback (no-bash hosts).
		command = warmupCmd("powershell") + "; " + command
	}

	root := m.cfg.Workdir
	if pp, ok := workdir.PathPolicyFromContext(ctx); ok && pp.HasWorkdir() {
		root = pp.Root()
	}

	timeout := time.Duration(m.cfg.TimeoutSecs) * time.Second
	if p.TimeoutSeconds > 0 {
		timeout = time.Duration(p.TimeoutSeconds) * time.Second
	}
	// A background task is long-lived by nature (dev servers, watchers, tails):
	// it must NOT inherit the foreground timeout, or it gets killed mid-run and
	// reported as FAILED. With no explicit timeout it runs until it finishes or
	// the user/agent cancels it (the cancel path reaps its whole tree).
	if tool.IsBackground(ctx) && p.TimeoutSeconds <= 0 {
		timeout = 0
	}

	// EVERY command runs as its own one-shot process (cmd.Dir = workspace root),
	// never on a long-lived REPL. This is the design that makes the shell robust
	// — same as opencode, Claude Code, and a plain `bash -c`:
	//   • a malformed command CANNOT poison the next (no continuation-state to
	//     swallow the following command → no cascade timeouts) ;
	//   • the agent's real bash runs NATIVELY (no &&→PS / 2>/dev/null / export
	//     translation, no base64 dot-source frame, no nested-quoting collisions) ;
	//   • stdout / stderr / exit code are captured by the OS, not parsed back out
	//     of a tagged stream.
	// The trade-off, accepted on purpose : shell STATE (cd / export / venv / funcs)
	// does NOT carry across calls. Every call starts at the workspace root, so the
	// agent sets things up inline in ONE command (`cd proj && npm install`,
	// `source venv/bin/activate && pytest`).
	started := time.Now()
	res, err := runDetached(ctx, m.kind, m.path, command, root, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, p.Input, timeout)
	if err != nil && !errors.Is(err, errTimeout) && !errors.Is(err, errCancelled) {
		return errResult(err), nil
	}
	if res.Cwd == "" {
		res.Cwd = root
	}
	m.enrich(&res, root, started)
	workdir.NotifyFileChange(ctx) // a command may mutate the tree; shadow-repo git status is the arbiter
	return m.result(command, res, err, timeout), nil
}

// anchorCmd prefixes the command with a cd back to the workspace root so every
// call starts from the same fixed directory. The persistent shell keeps env
// vars, exports, venv activation and shell functions alive between calls — only
// the working directory is reset, so a weaker model can't be tripped by a cwd
// that silently drifted under it. No subshell is used (the cd and the command
// share the shell's `{ }` group in frame()), so the command's own exports still
// persist. The root is normalised because msys bash's `cd` takes forward-slash
// drive paths (C:/Users/...) — backslashes are escape characters there.
func anchorCmd(kind, root, command string) string {
	if root == "" {
		return command
	}
	if kind == "powershell" {
		// Escape ' as '' for a PowerShell single-quoted literal so a root path
		// with an apostrophe (C:\Users\O'Brien\...) can't break out / inject.
		return "Set-Location -LiteralPath '" + strings.ReplaceAll(root, "'", "''") + "'; " + command
	}
	// POSIX single-quoted literal: escape ' as '\'' for the same reason.
	fwd := strings.ReplaceAll(root, "\\", "/")
	fwd = strings.ReplaceAll(fwd, "'", `'\''`)
	return "cd '" + fwd + "' && " + command
}

// shellName labels which backend ran the command, so the agent isn't guessing
// whether it's on real bash or the built-in interpreter.
func (m *Module) shellName() string {
	if m.useGoShell {
		return "goshell"
	}
	if m.kind != "" {
		return m.kind
	}
	return "sh"
}

func (m *Module) result(command string, res cmdResult, err error, timeout time.Duration) tool.Result {
	out := tool.Result{
		Success: res.ExitCode == 0 && !res.TimedOut && !res.Cancelled && !errors.Is(err, errShellExited),
		Data: runResult{
			Stdout:       res.Stdout,
			Stderr:       res.Stderr,
			ExitCode:     res.ExitCode,
			Cwd:          res.Cwd,
			Shell:        m.shellName(),
			TimedOut:     res.TimedOut,
			Cancelled:    res.Cancelled,
			DurationMs:   res.DurationMs,
			FilesChanged: res.FilesChanged,
			FilesNote:    res.FilesNote,
			Git:          res.Git,
		},
		Display: &tool.DisplayHint{Type: "text", Title: firstToken(command)},
	}
	if !out.Success {
		switch {
		case res.Cancelled:
			out.Error = "command cancelled; its process tree was killed"
		case res.TimedOut:
			out.Error = fmt.Sprintf("command timed out after %s; its process tree was killed", timeout)
		case errors.Is(err, errShellExited):
			out.Error = "the command ended the shell session (e.g. `exit`); a fresh shell starts on the next call"
		default:
			// Surface the REASON in the error itself, not just "exit code N".
			// stderr (then stdout) carries the actual cause — ParserError,
			// IndentationError, ModuleNotFoundError, "not recognized", … — and a
			// weak agent fixates on the error field, so an opaque code makes it
			// flail blindly. The full output still rides in Data ; this is the
			// unmissable summary.
			out.Error = fmt.Sprintf("exit code %d", res.ExitCode)
			if d := errorDetail(res.Stderr, res.Stdout); d != "" {
				out.Error += ": " + d
			}
		}
	}
	return out
}

// errorDetail extracts the most useful failure text for the error field —
// stderr first, else stdout — keeping the TAIL (errors print last) and capping
// it so the cause is visible without flooding the agent's context.
func errorDetail(stderr, stdout string) string {
	d := strings.TrimSpace(stderr)
	if d == "" {
		d = strings.TrimSpace(stdout)
	}
	if d == "" {
		return ""
	}
	if lines := strings.Split(d, "\n"); len(lines) > 15 {
		d = strings.Join(lines[len(lines)-15:], "\n")
	}
	if len(d) > 1500 {
		d = "…" + d[len(d)-1500:]
	}
	return d
}

func (m *Module) getShell(key, dir string) (*shell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sh, ok := m.shells[key]; ok && !sh.isClosed() {
		return sh, nil
	}
	sh, err := newShellResilient(m.kind, m.path, dir, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput)
	if err != nil {
		return nil, err
	}
	m.shells[key] = sh
	return sh, nil
}

// dropShell removes the SPECIFIC shell we ran on from the map and closes it.
// It compares identity before deleting so a concurrent getShell for the same
// session that already installed a fresh replacement is NOT clobbered (the old
// delete-by-key closed whatever was at the key — destroying a healthy new
// shell). Closing `sh` itself is always safe and idempotent.
func (m *Module) dropShell(key string, sh *shell) {
	m.mu.Lock()
	if cur, ok := m.shells[key]; ok && cur == sh {
		delete(m.shells, key)
	}
	m.mu.Unlock()
	sh.close()
}

func (m *Module) PromptSections(scope domainmodule.PromptScope) []domainmodule.PromptSection {
	osName := map[string]string{"windows": "Windows", "darwin": "macOS", "linux": "Linux"}[goruntime.GOOS]
	if osName == "" {
		osName = goruntime.GOOS
	}
	shell, syntax := "bash", "Use POSIX shell syntax (`&&` chaining, `$VAR`, `2>&1`)."
	sysHint := "List processes with `ps aux`; check a port with `lsof -i :PORT`."
	if m.kind == "powershell" {
		shell = "PowerShell"
		syntax = "This is PowerShell, NOT bash, but the common bash reflexes are auto-translated for you: `&&`/`||` " +
			"chaining (`cd proj && npm install`), `export VAR=value` and inline `VAR=value cmd`, and the null " +
			"redirects `2>/dev/null` / `2>nul` (and `>/dev/null 2>&1`). Cross-platform CLIs (npm, npx, node, git, " +
			"python, pip, go) and `$(...)` command substitution work identically. Still bash-ONLY and NOT supported — " +
			"use PowerShell or write a script file and run it: control-flow `for x in… / if [ … ] / while / test -f`, " +
			"`[[ … ]]`, heredocs (`<<EOF`), backticks `` `cmd` ``, and `source` (PowerShell dot-sources a `.ps1` with `.`). " +
			"If you write one of those you get a clear hint back, not a silent failure."
		sysHint = "List processes with `Get-Process` (or `tasklist`); check a port with `Get-NetTCPConnection -LocalPort PORT`."
	} else if goruntime.GOOS == "windows" {
		shell = "bash-compatible"
		syntax = "This is a BASH-compatible shell (a built-in POSIX interpreter), NOT cmd, NOT PowerShell, NOT Git-Bash/MSYS. " +
			"Use POSIX syntax: `&&` chaining, `$VAR`, `[[ … ]]`, `$(...)`, `2>&1`. " +
			"WINDOWS PATHS: write them with FORWARD slashes (`C:/Users/you/proj`) or QUOTE the backslash form (`\"C:\\Users\\you\\proj\"`); " +
			"an UNQUOTED `C:\\Users\\…` breaks (the backslashes are escapes), and `/c/Users/…` (MSYS style) does NOT work here. " +
			"Do NOT use cmd idioms: change directory with `cd <path>` — NEVER `cd /d` — and CLI flags are `-x`/`--x`, not `/x`. " +
			"The exception is native Windows programs (taskkill, robocopy, netstat, ipconfig): keep their `/F`, `/PID`, `/E` switches — they work as-is."
		sysHint = "List processes with `tasklist` (e.g. `tasklist | grep node`); check a port with `netstat -ano | grep :PORT`; kill a process by PID with `taskkill /F /PID <pid>`."
	}
	return []domainmodule.PromptSection{{
		Title:    "Shell (" + osName + " / " + shell + ")",
		Priority: 55,
		Content: "ENVIRONMENT: you are on " + osName + "; `bash.run` runs your command on a " + shell + " shell. " +
			"Use the shell to RUN PROGRAMS (npm, node, git, python, go, builds, tests, servers). " +
			"For FILE operations use the `filesystem` tools — read/glob/grep/edit/write — NOT the shell: `ls`/`cat`/`grep`/`find`/`head`/`tail` are redirected there. " +
			syntax + " " + sysHint + " " +
			"NO state persists between calls: every command is a fresh one-shot process starting at the workspace " +
			"root, so environment variables (`export VAR=...`), shell variables, functions, an activated venv AND " +
			"the working directory are all forgotten by the next call. Set up and use state in the SAME command — " +
			"`cd proj && npm install`, `source venv/bin/activate && pytest` — and write paths relative to the root; " +
			"never rely on a `cd`, `export` or `source` from a previous call. This also keeps paths unambiguous: " +
			"`cd proj` always means root/proj, never root/proj/proj. A backgrounded command likewise starts at the root. " +
			"It runs NON-INTERACTIVE (no TTY): pass flags instead of waiting for prompts, and chain " +
			"dependent steps with `&&`. For long-running work (builds, installs, full test runs, dev " +
			"servers, downloads) launch the command with `background_run` instead of blocking here. " +
			"background_run waits a short start-up window first: if the command FAILS immediately (a bad port, a " +
			"crash, a missing module) you get the error and its output RIGHT THEN, synchronously; if it is still " +
			"alive after the window it launched OK and keeps running in the background, and you are woken when it " +
			"later finishes or fails. Check a running task anytime with `background_run` passing its `task_id` " +
			"(state + captured output), or `{list_tasks:true}` to see them all, or cancel with `{task_id, cancel:true}`. " +
			"So do NOT claim a server is running until either the start-up window passed cleanly or a status check confirms it. " +
			"A command that NEVER returns on its own — a " +
			"dev server (`npm run dev`, `vite`), a watcher, `tail -f`, a REPL — MUST go through " +
			"`background_run`: never run it in the foreground (it freezes until the timeout) and never append " +
			"a trailing `&` (it pollutes the shell). Backgrounded commands have NO timeout. A backgrounded command runs in its own " +
			"process and can be stopped by the user from the client at any time — if that happens you are " +
			"notified instantly that it was cancelled, so check in rather than assuming it finished. Output " +
			"is captured and bounded; very large output is truncated with a marker.",
	}}
}

func errResult(err error) tool.Result {
	return tool.Result{Success: false, Error: err.Error()}
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
