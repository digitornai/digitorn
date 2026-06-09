package bash

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mbathepaul/digitorn/internal/modules/bash/goshell"
)

// backgroundAmpHint rejects a command whose last statement is a trailing `&`. A
// trailing `&` detaches the process from the daemon as an UNTRACKED orphan: no
// task_id, no captured logs, no signal when it dies, and it keeps holding its
// port — so a server "started" this way silently leaks and the agent is never
// told it crashed. The managed path (background_run) is the correct channel and
// gives all of that back. Returns "" when the command is fine.
func backgroundAmpHint(command string) string {
	if !goshell.TrailingBackground(command) {
		return ""
	}
	return "don't background with a trailing `&` — it detaches the process as an untracked orphan: no task_id, no captured logs, no notification when it dies, and it keeps holding its port (so the next launch hits EADDRINUSE). To run a server or any long-living command in the background, call `background_run` with the command WITHOUT the `&`: you get a task_id, a start-up check that surfaces an immediate crash, captured output, and a notification when it finishes or fails."
}

// foregroundServerPatterns match commands that NEVER return on their own — dev
// servers, preview servers, watchers. Run in the foreground they pin the turn
// until the timeout (default 900s) and then report a bogus failure when the kill
// finally lands; the cardinal rule is the loop is never blocked. They are
// matched on a QUOTE-MASKED copy (so `echo "npm run dev"` / `bash -c "vite"` are
// not flagged) and are deliberately conservative: nothing here matches a command
// that terminates on its own (build / test / install / lint / tsc), so a false
// positive never breaks a normal foreground step. `vite build` etc. stay allowed.
var foregroundServerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(npm|pnpm|yarn|bun)\s+(run\s+)?(dev|start|serve|preview|watch)\b`),
	regexp.MustCompile(`(?i)\bvite\s+(dev|serve|preview)\b`),
	regexp.MustCompile(`(?i)\bvite\s*($|[;&|])`), // bare `vite` / `npx vite` (defaults to the dev server)
	regexp.MustCompile(`(?i)\b(next|nuxt|remix)\s+(dev|start)\b`),
	regexp.MustCompile(`(?i)\bng\s+serve\b`),
	regexp.MustCompile(`(?i)\b(vue-cli-service|react-scripts)\s+(serve|start)\b`),
	regexp.MustCompile(`(?i)\bwebpack(-dev-server)?\s+serve\b`),
	regexp.MustCompile(`(?i)\bwebpack-dev-server\b`),
	regexp.MustCompile(`(?i)\bnodemon\b`),
	regexp.MustCompile(`(?i)\bflask\b.*\brun\b`),
	regexp.MustCompile(`(?i)\b(uvicorn|gunicorn|hypercorn|daphne|waitress-serve)\b`),
	regexp.MustCompile(`(?i)\bpython[0-9.]*\s+-m\s+http\.server\b`),
	regexp.MustCompile(`(?i)\bphp\s+-S\b`),
	regexp.MustCompile(`(?i)\brails\s+(server|s)\b`),
	regexp.MustCompile(`(?i)\b(http-server|live-server|serve)\b`),
	regexp.MustCompile(`(?i)\btail\s+-f\b`),
}

// foregroundServerHint rejects a long-running server / watcher run in the
// FOREGROUND (the caller skips this check when the dispatch is already a
// background_run). It returns "" for anything that isn't an unambiguous server.
// The message routes the agent to background_run AND states the fact that
// settles most of these calls: previewing the workspace needs no server at all —
// the build output is served automatically.
func foregroundServerHint(command string) string {
	masked := maskQuoted(command)
	for _, re := range foregroundServerPatterns {
		if re.MatchString(masked) {
			return "this looks like a long-running dev/preview server or watcher — it never returns on its own, so running it in the foreground freezes the turn until the timeout. Start it with `background_run` (no trailing `&`): you get a task_id, a startup check that surfaces an immediate crash, captured logs, and a notification when it exits. NOTE: you do NOT need to run a server to preview the app — the workspace serves the build output automatically. Just produce a production build (e.g. `npm run build`) and the preview attaches to it."
		}
	}
	return ""
}

// dosCommandEquivalents maps Windows cmd.exe commands that DON'T exist on a
// bash/sh shell to their bash equivalent. The model, seeing a Windows host,
// sometimes reaches for cmd syntax (`dir`, `copy`, `del`) and gets a cryptic
// "executable file not found" (exit 127) it then loops on. `dir` is included
// even though some Linux coreutils ship it — the Windows Git-Bash this targets
// has no `dir`, and a `dir` with cmd switches (`/B /S`) fails everywhere anyway.
// `type` is deliberately ABSENT: it is a real bash builtin (a legitimate call),
// so flagging it would block valid use.
var dosCommandEquivalents = map[string]string{
	"dir":     "ls (use `ls -R` to recurse, or `find . -type f` for a flat list)",
	"cls":     "clear",
	"copy":    "cp",
	"xcopy":   "cp -r",
	"del":     "rm",
	"erase":   "rm",
	"move":    "mv",
	"ren":     "mv",
	"rename":  "mv",
	"findstr": "grep",
}

// dosStatementSplit splits a command line into its top-level statements so the
// command WORD of each is checked (catches `cd app && dir /B /S`, where the bare
// command word is `cd`).
var dosStatementSplit = regexp.MustCompile(`[;&|]+`)

// dosCmdSwitch matches a cmd.exe-style single-letter switch token (`/B`, `/S`,
// `/Q`). Bash never uses these (flags are `-x`), and a one-letter `/X` is not a
// real path, so it is an unambiguous "the model thinks this is cmd.exe" signal.
var dosCmdSwitch = regexp.MustCompile(`(^|\s)/[A-Za-z](\s|$)`)

// dosHint returns a clear, actionable message when a command uses Windows
// cmd.exe syntax on a bash/sh shell (the caller skips it for a PowerShell
// target, which aliases dir/copy/del and has its own bashism translation).
// Runs on a quote-masked copy so a quoted literal (`echo "dir /B"`) is ignored.
func dosHint(command string) string {
	masked := maskQuoted(command)
	for _, seg := range dosStatementSplit.Split(masked, -1) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		if eq, ok := dosCommandEquivalents[strings.ToLower(fields[0])]; ok {
			return fmt.Sprintf("`%s` is a Windows cmd command — this is a bash shell, where it isn't available. Use `%s` instead. (Windows cmd commands like dir/copy/del/move/ren/cls have bash equivalents: ls/cp/rm/mv/mv/clear.)", strings.ToLower(fields[0]), eq)
		}
	}
	if dosCmdSwitch.MatchString(masked) {
		return "this is a bash shell, not Windows cmd.exe — `/B`, `/S`, `/Q`-style switches don't work here (bash flags are `-x`). e.g. `dir /B /S` → `find . -type f` (or `ls -R`)."
	}
	return ""
}

var hardDenied = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*r[a-z]*f?[a-z]*\s+/(\s|$|\*)`),
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*f[a-z]*r?[a-z]*\s+/(\s|$|\*)`),
	regexp.MustCompile(`:\s*\(\s*\)\s*\{`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\b.*\bof=/dev/(sd|hd|nvme|disk)`),
	regexp.MustCompile(`(?i)>\s*/dev/(sd|hd|nvme|disk)`),
}

// checkCommand is an advisory last line of defense only. The real barrier is
// workspace confinement (the shell starts inside the workspace) plus the env
// allowlist. It refuses a small set of unambiguously destructive patterns; it
// is NOT a sandbox and makes no completeness claim.
func checkCommand(command string) error {
	for _, re := range hardDenied {
		if re.MatchString(command) {
			return fmt.Errorf("command refused by safety guard (matched a destructive pattern)")
		}
	}
	return nil
}

// bashismPatterns match UNAMBIGUOUS bash syntax that PowerShell never produces,
// so detecting them can't false-positive on a real PowerShell command. Checked
// AFTER env-var translation (psEnv), so a translated `export`/inline-env no
// longer trips them — only genuinely un-runnable bash does.
var bashismPatterns = []*regexp.Regexp{
	regexp.MustCompile(`;\s*(then|do|done|fi|elif|esac)\b`),     // if/for/while/case bodies
	regexp.MustCompile(`\[\[?\s+-[a-zA-Z]\b`),                   // [ -f x ] / [[ -d y ]] test
	regexp.MustCompile(`(^|[;&|]|\bdo\b)\s*test\s+-[a-zA-Z]\b`), // test -f / test -d
	regexp.MustCompile(`(^|[;&|])\s*source\s`),                  // source venv/bin/activate
	regexp.MustCompile(`(^|[;&|])\s*export\s`),                  // export left untranslated ($-value)
}

// bashismHint returns a clear, actionable message when the command uses bash
// syntax that does NOT run on the PowerShell shell — so the agent gets guidance
// instead of a cryptic "[scriptblock]::Create" parse exception it would loop on.
// Returns "" when nothing unambiguously-bash is present.
//
// The check runs on a QUOTE-MASKED copy : bash syntax inside quotes is
// intentional (`bash -c "for x; do …; done"`, `echo "test -f …"`) and must NOT
// be flagged — explicitly invoking bash is the correct escape hatch, not an error.
func bashismHint(command string) string {
	masked := maskQuoted(command)
	for _, re := range bashismPatterns {
		if re.MatchString(masked) {
			return "this shell is PowerShell, not bash — that command uses bash-only syntax " +
				"(`if/for/while/[ ]/test`, `export`, or `source`) that won't run directly here. Either use " +
				"PowerShell (`$env:VAR='v'`, `foreach ($x in …) {…}`, `if (…) {…}`, `Test-Path`), or run the " +
				"exact bash via `bash -c '…'` — real bash is available; use SINGLE quotes so PowerShell does " +
				"not expand `$vars` before bash sees them — or write a script file with filesystem.write and " +
				"run it. Plain commands, pipes, `&&`, `||`, `$(…)`, `export VAR=v` and `2>/dev/null` already work."
		}
	}
	return ""
}

// maskQuoted blanks the contents of single/double-quoted spans (keeping the
// quote chars and overall length) so pattern matching ignores quoted text.
func maskQuoted(s string) string {
	b := []byte(s)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				b[i] = ' '
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
		}
	}
	return string(b)
}
