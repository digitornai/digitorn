package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/flexjson"
	"github.com/mbathepaul/digitorn/internal/modules/eventemitter"
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

	PromptWaitSecs int `json:"prompt_wait_secs" yaml:"prompt_wait_secs"`
}

type Module struct {
	module.Base
	cfg  Config
	kind string
	path string

	useGoShell bool

	useMvdanSh bool

	useWSL bool

	mu     sync.Mutex
	shells map[string]*shell

	stopJanitor chan struct{}
	janitorDone chan struct{}

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

func (m *Module) registerRunTool() {
	tail := runDescTail
	prompt := runToolPrompt
	if m.kind == "powershell" || m.kind == "pwsh" {
		tail = runDescTailPowerShell
		prompt = runToolPromptPowerShell
	}
	desc := runDescCore + " " + runDescStateless + " " + tail
	if m.envInfo != "" {
		desc += "\n\nThis host: " + m.envInfo + "."
	}
	m.RegisterTool(module.Tool{
		Name:         "run",
		Description:  desc,
		ToolPrompt:   prompt,
		Params:       runToolParams,
		Permissions:  []string{"bash.run"},
		RiskLevel:    tool.RiskHigh,
		Irreversible: true,
		Aliases:      []string{"bash", "sh", "shell"},
		Handler:      m.run,
	})
}

const (
	runDescCore = "Execute a shell command and return its stdout, stderr, exit code, working directory, duration, and git status."

	runDescStateless = "Shell state (cwd, env vars, sourced scripts, activated venvs, shell functions) PERSISTS across calls within the same agent session — exactly like a real terminal. `cd subdir` once and every subsequent command runs there. `source venv/bin/activate` once and it stays active. Use `&&` when one command must succeed before the next, but you no longer need to repeat setup every call."

	runDescTail = "The shell is mvdan/sh: a full POSIX + bash-compatible interpreter that spawns real OS processes (node, npm, git, python, go, docker, curl…) natively. It runs identically on Linux and macOS — no MSYS path rewriting, no Git Bash quirks. Supports pipelines, redirections (>, >>, 2>&1, 2>/dev/null), here-docs, arrays, [[ ]], $(...), &&/||/;. Commands that never return on their own (dev servers, watchers, tail -f, REPLs) MUST use background_run — never run them in the foreground (they freeze the turn until timeout) and never append a trailing &. Each result reports cwd, duration, files changed, and git branch/dirty status. Pass `pty: true` for commands that require a real terminal (docker run -it, ssh, programs that call isatty()) — runs inside a Unix PTY."

	runDescTailPowerShell = "The shell is the system PowerShell on Windows — a real REPL where state (cd, $variables, modules, functions) PERSISTS across calls within the session. Write PowerShell natively: cmdlets, pipeline objects, parameter binding. Bash idioms (&&, ||, /dev/null, `export VAR=…`, `VAR=… cmd`) are translated transparently so existing muscle memory keeps working — but you get the most power by speaking PowerShell directly. Native Windows commands (git, npm, node, python, docker, gh, taskkill, tasklist, netstat) work with their normal flags. Commands that never return on their own (dev servers, watchers, REPLs) MUST use background_run. Each result reports cwd, duration, files changed, and git branch/dirty status. Pass `pty: true` for commands that require a real console (docker run -it, ssh, winget interactive, az login) — runs inside a Windows ConPTY."
)

var runToolPrompt = "GOLDEN RULE: use dedicated tools for file operations — `read` instead of cat/head/tail, " +
	"`grep` instead of grep/rg, `glob` instead of find, `edit`/`write` instead of sed/awk/tee. " +
	"They render better, integrate with the UI, and never corrupt files. " +
	"Use the shell exclusively for running programs and driving the system.\n" +
	"\n" +
	"USE THE SHELL FOR:\n" +
	"• Build / test / install: `npm install`, `npm run build`, `go build ./...`, `pytest`, `cargo test`, `make`\n" +
	"• Git: `git status`, `git add -A && git commit -m 'msg'`, `git diff HEAD`, `git log --oneline -10`\n" +
	"• Running CLIs: curl, docker, kubectl, ffmpeg, jq, openssl, npx, pip, poetry, yarn…\n" +
	"• System inspection: ports, processes, env vars, disk, network\n" +
	"• Anything that produces output you need to act on\n" +
	"\n" +
	"SHELL CAPABILITIES (mvdan/sh — full bash-compatible, cross-platform):\n" +
	"• Pipelines:       `cmd1 | cmd2 | cmd3`\n" +
	"• Chaining:        `cmd1 && cmd2` (stop on fail) · `cmd1 || cmd2` (fallback) · `cmd1 ; cmd2` (always)\n" +
	"• Redirections:    `>file` · `>>file` · `2>&1` · `2>/dev/null` · `>/dev/null 2>&1`\n" +
	"• Substitution:    `$(cmd)` · `${VAR}` · `${VAR:-default}` · `${VAR:+alt}`\n" +
	"• Tests:           `[[ -f file ]]` · `[[ -d dir ]]` · `[[ -z $VAR ]]` · `[[ $A == $B ]]`\n" +
	"• Loops:           `for f in *.js; do echo \"$f\"; done` · `while read line; do …; done`\n" +
	"• Conditionals:    `if [[ … ]]; then …; elif …; fi`\n" +
	"• Here-docs:       `cat <<'EOF'\\n…\\nEOF`\n" +
	"• Arrays:          `arr=(a b c); echo ${arr[0]}; echo ${#arr[@]}`\n" +
	"• Arithmetic:      `$(( x + 1 ))` · `(( i++ ))`\n" +
	"• Functions:       `f() { …; }; f arg1`  (within a single command)\n" +
	"• Set flags:       `set -e` (exit on error) · `set -o pipefail`\n" +
	"\n" +
	"WINDOWS SPECIFICS (mvdan/sh handles these natively):\n" +
	"• Paths: always use forward slashes → `C:/Users/you/proj` NOT `C:\\Users\\you\\proj`\n" +
	"• Native Windows commands keep their flags: `taskkill /F /PID 1234`, `netstat /ano`\n" +
	"• Kill a port: `netstat -ano | grep :3000` then `taskkill /F /PID <pid>`\n" +
	"• List processes: `tasklist | grep node`\n" +
	"• No cmd.exe idioms: use `cd path` never `cd /d path`, flags are `-x` not `/x` for cross-platform CLIs\n" +
	"\n" +
	"BACKGROUND TASKS — background_run is MANDATORY for anything that never exits:\n" +
	"• Dev servers:  `npm run dev`, `next dev`, `vite`, `flask run`, `uvicorn`, `rails s`\n" +
	"• Watchers:     `tsc --watch`, `nodemon`, `cargo watch`\n" +
	"• Tails:        `tail -f logfile`\n" +
	"• REPLs:        `node`, `python`, `irb`\n" +
	"NEVER run these in the foreground (freezes the turn until timeout).\n" +
	"NEVER append & (orphans the process, untracked, no notification).\n" +
	"Check status:  background_run {task_id: \"…\"}\n" +
	"List all:      background_run {list_tasks: true}\n" +
	"Cancel:        background_run {task_id: \"…\", cancel: true}\n" +
	"Do NOT claim a server is running until the startup window passed cleanly or a status check confirms it.\n" +
	"\n" +
	"REPL AS SCRIPT — use heredoc instead of an interactive REPL (more powerful, no hang):\n" +
	"• Python:     bash.run(`python3 << 'PY'\\nimport pandas as pd; print(pd.__version__)\\nPY`)\n" +
	"• Node.js:    bash.run(`node << 'JS'\\nconsole.log(require('./package.json').version)\\nJS`)\n" +
	"• MySQL:      bash.run(`mysql -u root << 'SQL'\\nSHOW DATABASES; SELECT * FROM users LIMIT 5;\\nSQL`)\n" +
	"• PostgreSQL: bash.run(`psql -U postgres << 'SQL'\\n\\l\\nSELECT version();\\nSQL`)\n" +
	"• SQLite:     bash.run(`sqlite3 app.db << 'SQL'\\n.tables\\nSELECT COUNT(*) FROM users;\\nSQL`)\n" +
	"• Ruby:       bash.run(`ruby << 'RB'\\nrequire 'json'; puts JSON.pretty_generate({a:1})\\nRB`)\n" +
	"Heredoc runs the entire script in one shot — faster, richer, and never hangs.\n" +
	"\n" +
	"RELIABILITY RULES:\n" +
	"• Quote paths with spaces: `cd \"My Project\" && npm install`\n" +
	"• Chain dependent steps: `npm ci && npm run build && npm test` — fail fast\n" +
	"• Non-interactive flags: `npm install --yes`, `pip install --quiet`, `apt-get install -y`\n" +
	"• Check before destroying: state what `rm -rf`, `git reset --hard`, `git push --force` will do\n" +
	"• Never exfiltrate secrets or pipe credentials to the network\n" +
	"• Prefer explicit paths over relying on cwd drift between calls"

var runToolPromptPowerShell = "GOLDEN RULE: use dedicated tools for file operations — `read` instead of cat/Get-Content, " +
	"`grep` instead of grep/Select-String, `glob` instead of Get-ChildItem -Recurse, `edit`/`write` instead of Set-Content/Add-Content. " +
	"They render better, integrate with the UI, and never corrupt files. " +
	"Use the shell exclusively for running programs and driving the system.\n" +
	"\n" +
	"THIS SHELL IS POWERSHELL (Windows native). Cmdlets, pipeline OBJECTS (not text), parameter binding, " +
	"and the .NET surface are all directly available. Bash idioms below ARE translated transparently, but " +
	"native PowerShell unlocks the full power of the host — prefer it.\n" +
	"\n" +
	"USE THE SHELL FOR:\n" +
	"• Build / test / install: `npm install`, `npm run build`, `go build ./...`, `pytest`, `cargo test`\n" +
	"• Git: `git status`, `git add -A; git commit -m 'msg'`, `git diff HEAD`, `git log --oneline -10`\n" +
	"• Running CLIs: curl, docker, kubectl, ffmpeg, jq, openssl, npx, pip, poetry, yarn, gh, az…\n" +
	"• System inspection: ports, processes, services, env vars, disk, network\n" +
	"• Anything that produces output you need to act on\n" +
	"\n" +
	"NATIVE POWERSHELL — the most powerful surface:\n" +
	"• Chaining:        `cmd1 -and cmd2` is NOT a thing — use `; if ($?) { cmd2 }` OR rely on `cmd1 && cmd2` (we translate it)\n" +
	"• Pipeline objects: `Get-ChildItem | Where-Object Name -like '*.go' | ForEach-Object { $_.FullName }`\n" +
	"• Filter / map:    `… | Where-Object { $_.Size -gt 1MB }` · `… | Select-Object -First 10`\n" +
	"• Sort / group:    `… | Sort-Object LastWriteTime -Descending` · `… | Group-Object Extension`\n" +
	"• Format / table:  `… | Format-Table Name,Length -AutoSize` (output only — for capture stay as objects)\n" +
	"• String ops:      `'foo bar' -split ' '` · `'foo' -replace 'o','0'` · `'abc' -match '^a'`\n" +
	"• Env vars:        `$env:VAR = 'value'` · `$env:PATH -split ';'`\n" +
	"• Test files:      `Test-Path file` · `Test-Path file -PathType Leaf`\n" +
	"• Read JSON:       `Get-Content file.json | ConvertFrom-Json`\n" +
	"• Write JSON:      `$obj | ConvertTo-Json -Depth 10 | Set-Content out.json`\n" +
	"• HTTP:            `Invoke-RestMethod -Uri https://api.example.com/foo -Method GET -Headers @{Authorization='Bearer x'}`\n" +
	"• Try / catch:     `try { … } catch { Write-Error $_; exit 1 }`\n" +
	"• Strict mode:     `Set-StrictMode -Version Latest; $ErrorActionPreference = 'Stop'`\n" +
	"\n" +
	"BASH IDIOMS THAT WORK HERE (translated for you):\n" +
	"• `cmd1 && cmd2`  → runs cmd2 only if cmd1 succeeded\n" +
	"• `cmd1 || cmd2`  → runs cmd2 only if cmd1 failed\n" +
	"• `>/dev/null`    → silence stdout · `2>/dev/null` silences stderr · `>/dev/null 2>&1` silences both\n" +
	"• `export VAR=v`  → sets `$env:VAR` for the session\n" +
	"• `VAR=v cmd`     → inline env (one-shot)\n" +
	"\n" +
	"WHERE POWERSHELL DIFFERS FROM BASH (use the right side):\n" +
	"• Conditionals:    `if (Test-Path foo) { … } elseif (…) { … } else { … }`  (NOT `if [[ … ]]; then; fi`)\n" +
	"• Loops:           `foreach ($f in Get-ChildItem *.go) { Write-Output $f.Name }`\n" +
	"• Arrays:          `$arr = @('a','b','c'); $arr[0]; $arr.Count`\n" +
	"• Here-strings:    `$text = @'\\n…\\n'@`  (single-quoted = literal, no $ expansion)\n" +
	"• Backticks:       `` ` `` is the LINE-CONTINUATION character, NOT command substitution. Use `$(cmd)` for substitution\n" +
	"• Subshell:        `& { …; … }` (child scope) or `. { …; … }` (current scope, keeps state)\n" +
	"\n" +
	"POWERSHELL SUPERPOWERS (unavailable in bash — use these for maximum leverage on Windows):\n" +
	"• .NET inline:    `[System.Net.Dns]::GetHostAddresses('google.com') | Select-Object -Expand IPAddressToString`\n" +
	"• REST native:    `(Invoke-RestMethod 'https://api.github.com/repos/org/repo').stargazers_count`\n" +
	"• WMI/CIM:        `Get-CimInstance Win32_Processor | Select-Object Name,NumberOfCores,MaxClockSpeed`\n" +
	"• Registry:       `Get-ItemProperty 'HKLM:\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion' | Select-Object ProgramFilesDir`\n" +
	"• Services:       `Get-Service | Where-Object Status -eq Running | Select-Object Name,DisplayName | Sort-Object Name`\n" +
	"• Event logs:     `Get-EventLog -LogName Application -Newest 20 -EntryType Error`\n" +
	"• COM objects:    `$ie = New-Object -ComObject InternetExplorer.Application`\n" +
	"• Type casting:   `[datetime]'2024-01-01'` · `[int]'42'` · `[xml](Get-Content file.xml)`\n" +
	"• Parallel:       `1..10 | ForEach-Object -Parallel { Start-Sleep 1; $_ } -ThrottleLimit 5`\n" +
	"• Secure creds:   `$cred = Get-Credential; Invoke-Command -Credential $cred -ComputerName server { hostname }`\n" +
	"• SecureString:   `ConvertTo-SecureString 'pwd' -AsPlainText -Force | ConvertFrom-SecureString`\n" +
	"• ACL:            `Get-Acl 'C:\\path' | Format-List`\n" +
	"\n" +
	"REPL AS SCRIPT in PowerShell — use one-liners or here-strings instead of interactive REPL:\n" +
	"• Python:    `python -c \"import sys; print(sys.version)\"`\n" +
	"• Node:      `node -e \"console.log(require('./package.json').version)\"`\n" +
	"• MySQL:     `mysql -u root -e 'SHOW DATABASES;'`\n" +
	"• SQLite:    `sqlite3 db.sqlite '.tables'`\n" +
	"• PS script: `$out = @'\\nSELECT * FROM users LIMIT 5;\\n'@ | mysql -u root mydb; $out`\n" +
	"\n" +
	"WINDOWS-NATIVE TIPS:\n" +
	"• Ports:           `Get-NetTCPConnection -State Listen | Where-Object LocalPort -eq 3000` then `Stop-Process -Id $_.OwningProcess -Force`\n" +
	"• Processes:       `Get-Process node` · `Stop-Process -Name node -Force`\n" +
	"• Services:        `Get-Service` · `Restart-Service -Name <name>`\n" +
	"• Paths:           prefer forward slashes (`C:/Users/you/proj`) — PowerShell and every modern Windows CLI accept them\n" +
	"• Quoting:         single quotes are literal; double quotes interpolate `$vars` — use single quotes whenever you don't need expansion\n" +
	"\n" +
	"BACKGROUND TASKS — background_run is MANDATORY for anything that never exits:\n" +
	"• Dev servers:  `npm run dev`, `next dev`, `vite`, `flask run`, `uvicorn`, `rails s`\n" +
	"• Watchers:     `tsc --watch`, `nodemon`, `cargo watch`\n" +
	"• Tails:        `Get-Content -Wait logfile` / `tail -f`\n" +
	"• REPLs:        `node`, `python`, `pwsh`\n" +
	"NEVER run these in the foreground (freezes the turn until timeout).\n" +
	"NEVER append & (orphans the process, untracked, no notification).\n" +
	"Check status:  background_run {task_id: \"…\"}\n" +
	"List all:      background_run {list_tasks: true}\n" +
	"Cancel:        background_run {task_id: \"…\", cancel: true}\n" +
	"Do NOT claim a server is running until the startup window passed cleanly or a status check confirms it.\n" +
	"\n" +
	"RELIABILITY RULES:\n" +
	"• Quote paths with spaces: `cd 'My Project'; npm install`\n" +
	"• Chain dependent steps:   `npm ci && npm run build && npm test` (translated to ; with $? checks)\n" +
	"• Non-interactive flags:   `npm install --yes`, `pip install --quiet`, `winget install -h`\n" +
	"• Check before destroying: state what `Remove-Item -Recurse -Force`, `git reset --hard`, `git push --force` will do\n" +
	"• Never exfiltrate secrets or pipe credentials to the network\n" +
	"• Prefer explicit paths over relying on cwd drift between calls"

var runToolParams = []tool.ParamSpec{
	{Name: "command", Type: "string", Description: "The shell command line to execute.", Required: true},
	{Name: "timeout_seconds", Type: "integer", Description: "Per-call timeout in seconds; the running command's process tree is killed on expiry. 0 = module default (900s).", Default: 0},
	{Name: "input", Type: "string", Description: "Text fed to the command's stdin — use it to answer prompts or pipe data in (e.g. \"y\\n\"). The command runs as its own one-shot process.", Required: false},
	{Name: "pty", Type: "boolean", Description: "Run inside a pseudo-terminal (PTY on Linux/macOS, ConPTY on Windows). Required for programs that check isatty(): docker run -it, ssh, winget interactive, npm login, az login. PTY merges stdout+stderr and strips ANSI codes. Falls back to regular subprocess if PTY is unavailable.", Default: false},
}

func (m *Module) HasShell() bool { return m.path != "" }

func (m *Module) Kind() string { return m.kind }

func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	if cfg != nil {
		raw, _ := json.Marshal(cfg)
		_ = json.Unmarshal(raw, &m.cfg)
	}
	if m.cfg.MaxOutput <= 0 {
		m.cfg.MaxOutput = 100 << 10
	}
	if m.cfg.TimeoutSecs <= 0 {
		m.cfg.TimeoutSecs = 900
	}
	if m.cfg.IdleSecs <= 0 {
		m.cfg.IdleSecs = 900
	}
	if m.cfg.PromptWaitSecs <= 0 {
		m.cfg.PromptWaitSecs = 15
	}

	prefer := m.cfg.Shell
	if env := strings.TrimSpace(os.Getenv("DIGITORN_BASH_PATH")); env != "" {
		prefer = env
	}

	if strings.EqualFold(prefer, "goshell") || strings.EqualFold(prefer, "go") {
		m.useGoShell = true
		m.finalizeInit()
		return nil
	}

	if strings.EqualFold(prefer, "mvdan") || strings.EqualFold(prefer, "mvdan/sh") {
		m.useMvdanSh = true
		m.finalizeInit()
		return nil
	}

	if strings.EqualFold(prefer, "wsl") || strings.EqualFold(prefer, "wsl-bash") {
		if wslExe := detectWSLExe(); wslExe != "" {
			m.kind = "bash"
			m.path = wslExe
			m.useWSL = true
			m.finalizeInit()
			return nil
		}
	}

	if goruntime.GOOS == "windows" && prefer == "" {
		kind, path, err := detectShell("powershell")
		if err == nil {
			m.kind, m.path = kind, path
		} else {
			kind, path, err = detectShell("pwsh")
			if err == nil {
				m.kind, m.path = kind, path
			} else {
				m.useMvdanSh = true
			}
		}
		m.finalizeInit()
		return nil
	}

	kind, path, err := detectShell(prefer)
	if err == nil {
		m.kind, m.path = kind, path
		if goruntime.GOOS == "windows" && m.kind == "bash" && strings.HasSuffix(strings.ToLower(path), "wsl.exe") {
			m.useWSL = true
		}
	}
	if m.kind != "bash" && m.kind != "sh" {
		m.useMvdanSh = true
	}

	m.finalizeInit()
	return nil
}

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
				defer m.mu.Unlock()
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
	Command        string       `json:"command"`
	TimeoutSeconds flexjson.Int `json:"timeout_seconds"`
	Input          string       `json:"input"`
	PTY            flexjson.Bool `json:"pty"`
}

type runResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Cwd       string `json:"cwd"`
	Shell     string `json:"shell"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`

	WaitingForInput bool `json:"waiting_for_input,omitempty"`

	DurationMs   int64    `json:"duration_ms,omitempty"`
	FilesChanged []string `json:"files_changed,omitempty"`
	FilesNote    string   `json:"files_note,omitempty"`
	Git          *gitInfo `json:"git,omitempty"`
}

func (m *Module) DeclaredEvents() []map[string]string {
	return []map[string]string{
		{"topic": "bash.command.executed", "type": "command.executed"},
		{"topic": "bash.command.failed", "type": "command.failed"},
		{"topic": "bash.command.timed_out", "type": "command.timed_out"},
	}
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
	if msg := backgroundAmpHint(command); msg != "" {
		return errResult(errors.New(msg)), nil
	}
	if !tool.IsBackground(ctx) {
		if msg := foregroundServerHint(command); msg != "" {
			return errResult(errors.New(msg)), nil
		}
	}
	if m.useGoShell || m.useMvdanSh || m.kind != "powershell" {
		if msg := dosHint(command); msg != "" {
			return errResult(errors.New(msg)), nil
		}
	}

	if !p.PTY && p.Input == "" && m.path != "" {
		p.PTY = flexjson.Bool(needsPTY(command))
	}

	if p.PTY && m.path != "" && p.Input == "" {
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
		// Prompt watchdog active in the foreground (where a hang freezes the turn);
		// disabled for background tasks, which run off the loop and may sit quiet.
		promptWait := time.Duration(m.cfg.PromptWaitSecs) * time.Second
		if tool.IsBackground(ctx) {
			promptWait = 0
		}
		res := runWithPTY(ctx, m.kind, m.path, command, root, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, timeout, promptWait)
		m.enrich(&res, root, started)
		workdir.NotifyFileChange(ctx)
		return m.result(command, res, nil, timeout), nil
	}

	if m.useMvdanSh {
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
		res := runMvdanSh(ctx, command, root, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, p.Input, timeout)
		m.enrich(&res, root, started)
		workdir.NotifyFileChange(ctx)
		return m.result(command, res, nil, timeout), nil
	}

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
		workdir.NotifyFileChange(ctx)
		return m.result(command, res, nil, timeout), nil
	}

	if m.path == "" {
		return errResult(errors.New("no shell available: bash or sh must be on PATH")), nil
	}
	if m.kind == "powershell" {
		command = psNulSink(psChain(psEnv(command)))
		if msg := bashismHint(command); msg != "" {
			return errResult(errors.New(msg)), nil
		}
	}

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

	sessionKey := root
	if id, ok := tool.IdentityFromContext(ctx); ok && id.SessionID != "" {
		sessionKey = id.SessionID + ":" + root
	}

	started := time.Now()
	var res cmdResult
	if p.Input != "" {
		var rerr error
		res, rerr = runDetached(ctx, m.kind, m.path, command, root, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, p.Input, timeout, 0)
		if rerr != nil && !errors.Is(rerr, errTimeout) && !errors.Is(rerr, errCancelled) && !errors.Is(rerr, errWaitingForInput) {
			return errResult(rerr), nil
		}
	} else {
		res = m.runInSession(ctx, sessionKey, root, command, timeout)
	}
	if res.Cwd == "" {
		res.Cwd = root
	}
	m.enrich(&res, root, started)
	workdir.NotifyFileChange(ctx)

	if res.TimedOut {
		eventemitter.EmitWithModule(ctx, "bash", "bash.command.timed_out", map[string]any{
			"command":    command,
			"duration_ms": res.DurationMs,
		})
	} else if res.ExitCode != 0 {
		eventemitter.EmitWithModule(ctx, "bash", "bash.command.failed", map[string]any{
			"command":    command,
			"exit_code":  res.ExitCode,
			"duration_ms": res.DurationMs,
		})
	} else {
		eventemitter.EmitWithModule(ctx, "bash", "bash.command.executed", map[string]any{
			"command":    command,
			"exit_code":  res.ExitCode,
			"duration_ms": res.DurationMs,
		})
	}

	return m.result(command, res, nil, timeout), nil
}

func wslQuotePath(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

func (m *Module) runInSession(ctx context.Context, key, dir, command string, timeout time.Duration) cmdResult {
	if m.useWSL {
		wrapped := "cd " + wslQuotePath(dir) + " && " + command
		res, _ := runDetached(ctx, "bash", m.path, wrapped, dir, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, "", timeout, 0)
		return res
	}
	if tool.IsBackground(ctx) {
		res, _ := runDetached(ctx, m.kind, m.path, command, dir, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, "", timeout, 0)
		return res
	}
	sh, err := m.getShell(key, dir)
	if err != nil {
		res, _ := runDetached(ctx, m.kind, m.path, command, dir, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, "", timeout, time.Duration(m.cfg.PromptWaitSecs)*time.Second)
		return res
	}
	res, err := sh.run(ctx, command, timeout)
	if err != nil {
		if errors.Is(err, errShellExited) {
			m.dropShell(key, sh)
		}
		if res.Stdout == "" && res.Stderr == "" && res.ExitCode == 0 {
			res2, _ := runDetached(ctx, m.kind, m.path, command, dir, buildEnv(m.cfg.EnvAllow), m.cfg.MaxOutput, "", timeout, time.Duration(m.cfg.PromptWaitSecs)*time.Second)
			return res2
		}
	}
	return res
}

func anchorCmd(kind, root, command string) string {
	if root == "" {
		return command
	}
	if kind == "powershell" {
		return "Set-Location -LiteralPath '" + strings.ReplaceAll(root, "'", "''") + "'; " + command
	}
	fwd := strings.ReplaceAll(root, "\\", "/")
	fwd = strings.ReplaceAll(fwd, "'", `'\''`)
	return "cd '" + fwd + "' && " + command
}

func (m *Module) shellName() string {
	if m.useMvdanSh {
		return "mvdan/sh"
	}
	if m.useGoShell {
		return "goshell"
	}
	if m.kind != "" {
		return m.kind
	}
	return "sh"
}

func (m *Module) result(command string, res cmdResult, err error, timeout time.Duration) tool.Result {
	toolFailed := res.TimedOut || res.Cancelled || res.WaitingForInput || res.ExitCode != 0 ||
		errors.Is(err, errShellExited) || errors.Is(err, errWaitingForInput)
	out := tool.Result{
		Success: !toolFailed,
		Data: runResult{
			Stdout:          res.Stdout,
			Stderr:          res.Stderr,
			ExitCode:        res.ExitCode,
			Cwd:             res.Cwd,
			Shell:           m.shellName(),
			TimedOut:        res.TimedOut,
			Cancelled:       res.Cancelled,
			WaitingForInput: res.WaitingForInput,
			DurationMs:      res.DurationMs,
			FilesChanged:    res.FilesChanged,
			FilesNote:       res.FilesNote,
			Git:             res.Git,
		},
		Display: &tool.DisplayHint{Type: "text", Title: firstToken(command)},
	}
	if toolFailed {
		switch {
		case res.Cancelled:
			out.Error = "command cancelled; its process tree was killed"
		case res.TimedOut:
			out.Error = fmt.Sprintf("command timed out after %s; its process tree was killed", timeout)
		case errors.Is(err, errShellExited):
			out.Error = "the command ended the shell session (e.g. `exit`); a fresh shell starts on the next call"
		case res.WaitingForInput || errors.Is(err, errWaitingForInput):
			out.Error = "command was waiting for interactive input (e.g. a sudo/ssh password or a y/n confirmation) and was stopped so it could not hang. Retry with the `input` parameter to provide the answer, or use a non-interactive form (e.g. `sudo -n`, `--yes`/`-y`, `ssh -o BatchMode=yes`)."
		default: // non-zero exit code: surface the code AND the reason (stderr) so the agent sees WHY
			if detail := errorDetail(res.Stderr, res.Stdout); detail != "" {
				out.Error = fmt.Sprintf("exit code %d: %s", res.ExitCode, detail)
			} else {
				out.Error = fmt.Sprintf("exit code %d", res.ExitCode)
			}
		}
	}
	return out
}

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

	shellLabel := "bash"
	syntaxHint := "Full POSIX + bash syntax: `&&`/`||`/`;` chaining, `$VAR`, `[[ … ]]`, `$(…)`, `2>&1`, `2>/dev/null`, arrays, here-docs, loops, functions."
	sysHint := "List processes: `ps aux | grep node`. Check a port: `lsof -i :3000`. Kill by PID: `kill -9 <pid>`."

	if m.kind == "powershell" {
		shellLabel = "PowerShell"
		syntaxHint = "**ALWAYS USE NATIVE POWERSHELL COMMANDS.** This shell is PowerShell, not bash. The bash-to-PowerShell translation (`&&`→`if ($?)`, `export VAR=v`→`$env:VAR='v'`, `2>/dev/null`→`2>$null`) is a LIMITED safety net — do NOT rely on it. Write native PowerShell commands instead:\n" +
			"• Chaining:  `cmd1; if ($?) { cmd2 }` instead of `cmd1 && cmd2`\n" +
			"• Env vars:  `$env:VAR = 'v'` instead of `export VAR=v`\n" +
			"• Suppress:  `2>$null` or `>$null` instead of `2>/dev/null`\n" +
			"• Pipeline:  `Get-Process | Where-Object { $_.CPU -gt 10 }`\n" +
			"• Processes: `Get-Process`, `Stop-Process -Id <pid>`\n" +
			"• Ports:     `Get-NetTCPConnection -LocalPort 3000`\n" +
			"• Files:     `Get-ChildItem`, `Remove-Item -Recurse -Force`\n" +
			"• Services:  `Get-Service`, `Restart-Service`\n" +
			"• Commands:  `Get-Command`, `Get-Help`\n" +
			"The bash translation is ONLY for quick inline commands — for anything non-trivial, write proper PowerShell.\n" +
			"NOT supported at all (use PowerShell equivalents):\n" +
			"• Bash `[[ -f file ]]`, `[ -d dir ]`, `test -x` → use `Test-Path`, `if ($var -eq 'x')`\n" +
			"• `source venv/bin/activate` → use `venv/Scripts/Activate.ps1` or run absolute paths\n" +
			"• Heredocs `<<EOF`, backticks `` `cmd` `` → write a .ps1 script file instead\n" +
			"• Bash arrays `arr=(a b c)`, `${arr[@]}` → use PowerShell arrays `$arr = @('a','b','c')`\n" +
			"THE ESCAPE HATCH — if only bash syntax will do: `bash -c 'your bash command here'` (real bash, use single quotes to avoid PowerShell expanding `$vars` before bash sees them)."
		sysHint = "Windows system commands: `tasklist` (list processes) · `taskkill /F /PID <pid>` (kill) · `netstat -ano | Select-String :3000` (check port) · `where node` (find binary)."
	} else if m.useGoShell {
		shellLabel = "goshell (bash-compatible)"
		syntaxHint = "POSIX + bash syntax: `&&`/`||` chaining, `$VAR`, `[[ … ]]`, `$(…)`, `2>&1`."
		sysHint = "Built-in interpreter — external binaries must be on PATH."
	} else if m.useMvdanSh {
		shellLabel = "mvdan/sh (bash-compatible)"
		syntaxHint = "Full POSIX + bash syntax: `&&`/`||`/`;` chaining, `$VAR`, `[[ … ]]`, `$(…)`, `2>&1`, `2>/dev/null`, arrays, here-docs, loops. " +
			"External binaries (node, npm, git…) are spawned as real OS processes."
		if goruntime.GOOS == "windows" {
			syntaxHint += " WINDOWS PATHS: forward slashes always → `C:/Users/you/proj`. Native commands keep their flags: `taskkill /F /PID 1234`."
			sysHint = "List processes: `tasklist | grep node`. Check port: `netstat -ano | grep :3000`. Kill: `taskkill /F /PID <pid>`."
		}
	}

	return []domainmodule.PromptSection{{
		Title:    "Shell (" + osName + " / " + shellLabel + (func() string { if m.kind == "powershell" { return " — DEFAULT" }; return "" })() + ")",
		Priority: 55,
		Content: "ENVIRONMENT: " + osName + " · shell: " + shellLabel + ". " +
			"Use the shell to RUN PROGRAMS — builds, tests, installs, git, CLIs, servers. " +
			"For FILE operations use the filesystem tools (read/glob/grep/edit/write) — never cat/ls/find/grep from the shell. " +
			syntaxHint + " " + sysHint + " " +
			"NO STATE PERSISTS between calls: every command is a fresh one-shot process at the workspace root. " +
			"cd, export, venv activation, and shell functions are forgotten after each call. " +
			"Chain setup and use in ONE command (PowerShell syntax): `cd proj; npm install`, `npm ci; if ($?) { npm run build; if ($?) { npm test } }`. " +
			"LONG-RUNNING COMMANDS (dev servers, watchers, tail -f, REPLs) MUST go through background_run — " +
			"never run them in the foreground (freezes the turn until timeout) and never append & (orphans the process). " +
			"background_run waits a startup window: immediate failure → error now; still alive → confirmed running. " +
			"Check: background_run {task_id}. List: background_run {list_tasks:true}. Cancel: background_run {task_id, cancel:true}. " +
			"Output is captured and bounded; very large output is truncated with a marker.",
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
